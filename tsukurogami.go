package main

import (
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
	Phase      int         `json:"phase"`
	Body       string      `json:"body"`
	Name       string      `json:"name"`
	Type       int         `json:"type"`
	Conditions interface{} `json:"conditions"`
}

type Bot struct {
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
		err := createBot(repo[0], branch[0])
		w.WriteHeader(500)
		fmt.Fprintf(w, "error creating bot: %s\n", err)
	case "closed", "declined":
		err := deleteBot(repo[0], branch[0])
		w.WriteHeader(500)
		fmt.Fprintf(w, "error deleting bot: %s\n", err)
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

	url, err := url.Parse(*xcodeURL)
	if err != nil {
		return err
	}

	url.Path = path.Join(path.Join(url.Path, "api"), "bots")

	resp, err := client.Get(url.String())
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

	return nil
}

func deleteBot(repo, branch string) error {
	return nil
}

func main() {
	flag.Parse()
	client = &http.Client{Transport: transport{creds: *xcodeCredentials, Transport: http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *skipVerify}}}}

	createBot("iasmonitoring.ios10", "test")

	http.HandleFunc("/pullRequestUpdated", handlePullRequestUpdated)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
