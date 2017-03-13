package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
)

var switchBranch = `
#!/bin/sh
cd ${XCS_PRIMARY_REPO_DIR}
git fetch
git checkout %s
git pull # for good measure
`

var pokeStatus = `
#!/bin/sh
set -x
cd ${XCS_PRIMARY_REPO_DIR}
# TODO: Replace IP
curl "%s:%d/integrationUpdated?commit=$(git rev-parse HEAD | tr -d \n)&bot=${XCS_BOT_NAME}&integration=${XCS_INTEGRATION_NUMBER}&status=%s"
`

// flags
var (
	xcodeURL             = flag.String("xcode-url", "https://localhost:20343/", "The url of your xcode server")
	bitbucketURL         = flag.String("bitbucket-url", "", "The url of your bitbucket server")
	xcodeCredentials     = flag.String("xcode-credentials", "", "The credentials for your xcode server. username:password")
	bitbucketCredentials = flag.String("bitbucket-credentials", "", "The credentials for your bitbucket server. username:password")
	port                 = flag.Int("port", 4444, "The port to listen on")
	skipVerify           = flag.Bool("skip-verify", true, "Skip certification verification on the xcode server")
	template             = flag.String("template", "$REPO_NAME.continuous", "The bot from which settings should be copied")
)

var xcodeClient *http.Client
var bitbucketClient *http.Client

type transport struct {
	http.Transport
	creds string
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	auth := fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(t.creds)))
	req.Header.Add("Authorization", auth)
	return t.Transport.RoundTrip(req)
}

type Trigger struct {
	Phase              int              `json:"phase"`
	Body               string           `json:"scriptBody,omitempty"`
	Name               string           `json:"name"`
	Type               int              `json:"type"`
	EmailConfiguration *json.RawMessage `json:"emailConfiguration,omitempty"`
	Conditions         struct {
		OnAnalyzerWarnings bool `json:"onAnalyzerWarnings"`
		OnBuildErrors      bool `json:"onBuildErrors"`
		OnFailingTests     bool `json:"onFailingTests"`
		OnSuccess          bool `json:"onSuccess"`
		OnWarnings         bool `json:"onWarnings"`
		Status             int  `json:"status"`
	} `json:"conditions,omitempty"`
}

type Bot struct {
	ID     string        `json:"_id,omitempty"`
	Name   string        `json:"name"`
	Config Configuration `json:"configuration"`
}

type Configuration struct {
	m        map[string]*json.RawMessage
	triggers []Trigger
	envVars  map[string]interface{}
}

func (c Configuration) MarshalJSON() ([]byte, error) {
	triggerJSON, err := json.Marshal(c.triggers)
	if err != nil {
		return nil, err
	}
	envJSON, err := json.Marshal(c.envVars)
	if err != nil {
		return nil, err
	}
	t := json.RawMessage(triggerJSON)
	e := json.RawMessage(envJSON)
	c.m["triggers"] = &t
	c.m["buildEnvironmentVariables"] = &e
	return json.Marshal(c.m)
}

func (c *Configuration) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &c.m); err != nil {
		return err
	}

	if err := json.Unmarshal(*c.m["triggers"], &c.triggers); err != nil {
		return err
	}

	if err := json.Unmarshal(*c.m["buildEnvironmentVariables"], &c.envVars); err != nil {
		return err
	}

	return nil
}

func handlePullRequestUpdated(w http.ResponseWriter, r *http.Request) {

	repo, ok := r.URL.Query()["repo"]
	if !ok || len(repo) < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`missing "repo" parameter`))
		return
	}

	branch, ok := r.URL.Query()["branch"]
	if !ok || len(branch) < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`missing "branch" parameter`))
		return
	}

	status, ok := r.URL.Query()["status"]
	if !ok || len(status) < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`missing "status" parameter`))
		return
	}

	switch strings.ToLower(status[0]) {
	case "opened", "reopened":
		if err := createBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, "error creating bot: %s\n", err)
			return
		}
		if err := integrateBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, "error integrating newly created bot: %s\n", err)
			return
		}
	case "closed", "declined":
		if err := deleteBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, "error deleting bot: %s\n", err)
			return
		}
	case "rescoped_from":
		if err := integrateBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, "error integrating bot: %s\n", err)
			return
		}
	default:
		// nop
	}

	w.WriteHeader(201)
}

func handleIntegrationUpdated(w http.ResponseWriter, r *http.Request) {
	success := false
	defer func() {
		if success {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(500)
		}
	}()
	commit, ok := r.URL.Query()["commit"]
	if !ok || len(commit) < 1 {
		log.Println("no commit")
		return
	}
	status, ok := r.URL.Query()["status"]
	if !ok || len(status) < 1 {
		log.Println("no status")
		return
	}
	bot, ok := r.URL.Query()["bot"]
	if !ok || len(bot) < 1 {
		log.Println("no bot")
		return
	}

	integration, ok := r.URL.Query()["integration"]
	if !ok || len(integration) < 1 {
		log.Println("no integration")
		return
	}

	var state struct {
		State string `json:"state"`
		Key   string `json:"key"`
		Name  string `json:"name,omitempty"`
		URL   string `json:"url"`
		Desc  string `json:"description,omitempty"`
	}

	switch strings.ToLower(status[0]) {
	case "inprogress":
		state.State = "INPROGRESS"
	case "succeeded", "warnings":
		state.State = "SUCCESSFUL"
	case "trigger-error", "internal-build-error", "build-errors":
		state.State = "FAILED"
	default:
		state.State = "FAILED"
		state.Desc = "xcode returned: " + status[0]
	}
	state.Key = bot[0]
	state.Name = state.Key + ":" + integration[0]
	state.URL = "http://example.com/" // dunno what to do with this yet

	b, err := json.Marshal(state)
	if err != nil {
		log.Println(err)
		return
	}

	url, err := url.Parse(*bitbucketURL)
	if err != nil {
		return
	}
	url.Path = path.Join(path.Join(url.Path, "rest/build-status/1.0/commits/"), commit[0])
	resp, err := bitbucketClient.Post(url.String(), "application/json", bytes.NewReader(b))
	if err != nil {
		log.Println(err)
	}
	resp.Body.Close()
	success = true
}

func getBot(name string) (*Bot, error) {
	var botList struct {
		Count   int   `json:"count"`
		Results []Bot `json:"results"`
	}

	botsURL, err := url.Parse(*xcodeURL)
	if err != nil {
		return nil, err
	}

	botsURL.Path = path.Join(path.Join(botsURL.Path, "api"), "bots")

	resp, err := xcodeClient.Get(botsURL.String())
	if err != nil {
		return nil, err
	}
	if resp.Body == nil {
		return nil, errors.New("no response")
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(body, &botList)

	if botList.Count == 0 {
		return nil, errors.New("no bots")
	}

	var b *Bot
	for _, bot := range botList.Results {
		if strings.ToLower(bot.Name) == strings.ToLower(name) {
			b = &bot
			break
		}
	}

	if b == nil {
		return nil, errors.New("couldn't find template bot")
	}

	return b, nil
}

func createBot(repo, branch string) error {
	templateName := *template
	if templateName == "$REPO_NAME.continuous" {
		templateName = repo + ".continuous"
	}

	botsURL, err := url.Parse(*xcodeURL)
	if err != nil {
		return err
	}
	botsURL.Path = path.Join(path.Join(botsURL.Path, "api"), "bots")

	templateBot, err := getBot(templateName)
	if err != nil {
		return err
	}

	id := templateBot.ID

	prePoke := Trigger{Type: 1, Phase: 1, Name: "Update Status", Body: fmt.Sprintf(pokeStatus, *xcodeURL, *port, "inprogress")}
	postPoke := Trigger{Type: 1, Phase: 2, Name: "Update Status", Body: fmt.Sprintf(pokeStatus, *xcodeURL, *port, "${XCS_INTEGRATION_RESULT}")}
	postPoke.Conditions.OnWarnings = true
	postPoke.Conditions.OnSuccess = true
	postPoke.Conditions.OnFailingTests = true
	postPoke.Conditions.OnBuildErrors = true
	postPoke.Conditions.OnWarnings = true
	postPoke.Conditions.OnAnalyzerWarnings = true

	templateBot.Config.triggers = append([]Trigger{
		Trigger{Type: 1, Phase: 1, Name: "Switch Branch", Body: fmt.Sprintf(switchBranch, branch)},
		prePoke,
		postPoke,
	}, templateBot.Config.triggers...)
	templateBot.Config.envVars["TSUKUROGAMI_BRANCH"] = branch
	templateBot.Name = repo + "." + branch
	templateBot.ID = ""

	newJSON, err := json.Marshal(templateBot)

	if err != nil {
		return err
	}

	resp, err := xcodeClient.Post(fmt.Sprintf("%s/%s/duplicate", botsURL.String(), id), "application/json", bytes.NewReader(newJSON))
	if err != nil {
		return err
	}

	_ = resp

	return nil
}

func deleteBot(repo, branch string) error {
	botsURL, err := url.Parse(*xcodeURL)
	if err != nil {
		return err
	}
	botsURL.Path = path.Join(path.Join(botsURL.Path, "api"), "bots")
	b, err := getBot(repo + "." + branch)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/%s", botsURL.String(), b.ID), nil)
	if err != nil {
		return err
	}

	resp, err := xcodeClient.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != 204 {
		return errors.New("failed to delete")
	}

	return nil
}

func integrateBot(repo, branch string) error {
	bot, err := getBot(repo + "." + branch)
	if err != nil {
		return err
	}
	botsURL, err := url.Parse(*xcodeURL)
	if err != nil {
		return err
	}
	botsURL.Path = path.Join(path.Join(botsURL.Path, "api"), "bots")
	resp, err := xcodeClient.Post(fmt.Sprintf("%s/%s/integrations", botsURL.String(), bot.ID), "application/json", strings.NewReader(`{"shouldClean": true}`))
	if err != nil {
		return err
	}
	fmt.Println(resp.StatusCode)
	return nil
}

func main() {
	flag.Parse()
	xcodeClient = &http.Client{Transport: transport{creds: *xcodeCredentials, Transport: http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *skipVerify}}}}
	bitbucketClient = &http.Client{Transport: transport{creds: *bitbucketCredentials, Transport: http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *skipVerify}}}}

	http.HandleFunc("/pullRequestUpdated", handlePullRequestUpdated)
	http.HandleFunc("/integrationUpdated", handleIntegrationUpdated)
	log.Println(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
