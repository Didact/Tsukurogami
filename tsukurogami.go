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

// flags
var (
	xcodeURL             = flag.String("xcode-url", "", "The url of your xcode server")
	bitbucketURL         = flag.String("bitbucket-url", "", "The url of your bitbucket server")
	xcodeCredentials     = flag.String("xcode-credentials", "", "The credentials for your xcode server. username:password")
	bitbucketCredentials = flag.String("bitbucket-credentials", "", "The credentials for your bitbucket server. username:password")
	port                 = flag.Int("port", 4444, "The port to listen on")
	// configFilePath = flag.String("config", "", "(Optional) Path to configuration file")
	skipVerify = flag.Bool("skip-verify", true, "Skip certification verification on the xcode server")
	template   = flag.String("template", "$REPO_NAME.continuous", "The bot from which settings should be copied")
)

var client *http.Client

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
	Phase      int    `json:"phase"`
	Body       string `json:"scriptBody,omitempty"`
	Name       string `json:"name"`
	Type       int    `json:"type"`
	EmailConfiguration *json.RawMessage `json:"emailConfiguration,omitempty"`
	Conditions struct {
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
	case "closed", "declined":
		if err := deleteBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, "error deleting bot: %s\n", err)
			return
		}
	default:
		// nop
	}

	w.WriteHeader(201)
}

func createBot(repo, branch string) error {
	templateName := *template
	if templateName == "$REPO_NAME.continuous" {
		templateName = repo + ".continuous"
	}

	var botList struct {
		Count   int   `json:"count"`
		Results []Bot `json:"results"`
	}

	botsURL, err := url.Parse(*xcodeURL)
	if err != nil {
		return err
	}

	botsURL.Path = path.Join(path.Join(botsURL.Path, "api"), "bots")

	resp, err := client.Get(botsURL.String())
	if err != nil {
		return err
	}
	if resp.Body == nil {
		return errors.New("no response")
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	err = json.Unmarshal(body, &botList)

	if botList.Count == 0 {
		return errors.New("no bots")
	}

	var templateBot *Bot
	for _, bot := range botList.Results {
		if bot.Name == templateName {
			templateBot = &bot
			break
		}
	}

	if templateBot == nil {
		return errors.New("couldn't find template bot")
	}

	id := templateBot.ID

	templateBot.Config.triggers = append(templateBot.Config.triggers, Trigger{Type: 1, Phase: 1, Name: "Switch Branch", Body: fmt.Sprintf(switchBranch, branch)})
	templateBot.Config.envVars["TSUKUROGAMI_BRANCH"] = branch
	templateBot.Name = templateBot.Name + "." + branch
	templateBot.ID = ""

	newJSON, err := json.Marshal(templateBot)

	if err != nil {
		return err
	}

	resp, err = client.Post(fmt.Sprintf("%s/%s/duplicate", botsURL.String(), id), "application/json", bytes.NewReader(newJSON))
	if err != nil {
		return err
	}

	return nil
}

func deleteBot(repo, branch string) error {
	return nil
}

func main() {
	flag.Parse()
	client = &http.Client{Transport: transport{creds: *xcodeCredentials, Transport: http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *skipVerify}}}}

	http.HandleFunc("/pullRequestUpdated", handlePullRequestUpdated)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
