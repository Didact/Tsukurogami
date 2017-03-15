package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
)

var switchBranch = `
#!/bin/sh
set -x
cd ${XCS_PRIMARY_REPO_DIR}
git fetch
git checkout "%s"
git pull # for good measure
git merge --no-ff --no-commit master
`

var pokeStatus = `
#!/bin/sh
set -x
cd ${XCS_PRIMARY_REPO_DIR}
curl -g "%s:%d/integrationUpdated?commit=$(git rev-parse HEAD | tr -d \n)&bot=${XCS_BOT_NAME}&integration=${XCS_INTEGRATION_NUMBER}&status=%s"
`

type URL struct {
	// embedded because an alias requires too much casting IMO
	*url.URL
}

func (u URL) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.String())
}

func (u *URL) UnmarshalJSON(b []byte) error {
	var s string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return err
	}
	u2, err := url.Parse(s)
	if err != nil {
		return err
	}
	u.URL = u2
	return nil
}

func (u *URL) String() string {
	if u == nil || u.URL == nil {
		return ""
	}
	return u.URL.String()
}

func (u *URL) Set(s string) error {
	u2, err := url.Parse(s)
	if err != nil {
		return err
	}
	u.URL = u2
	return nil
}

var config struct {
	XcodeURL             URL    `json:"xcodeURL"`
	BitbucketURL         URL    `json:"bitbucketURL"`
	XcodeCredentials     string `json:"xcodeCredentials"`
	BitbucketCredentials string `json:"bitbucketCredentials"`
	Port                 int    `json:"port"`
	SkipVerify           bool   `json:"skipVerify"`
}

var myIP string

var xcodeClient *http.Client
var bitbucketClient *http.Client

type transport struct {
	*http.Transport
	creds string
}

func newTransport(creds string, skipVerify bool) transport {
	return transport{creds: creds, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerify}}}
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
	m            map[string]*json.RawMessage
	triggers     []Trigger
	envVars      map[string]interface{}
	scheduleType int
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
	scheduleJSON, err := json.Marshal(c.scheduleType)
	if err != nil {
		return nil, err
	}
	t := json.RawMessage(triggerJSON)
	e := json.RawMessage(envJSON)
	s := json.RawMessage(scheduleJSON)
	c.m["triggers"] = &t
	c.m["buildEnvironmentVariables"] = &e
	c.m["scheduleType"] = &s
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

	if err := json.Unmarshal(*c.m["scheduleType"], &c.scheduleType); err != nil {
		return err
	}

	return nil
}

type logger struct {
	m    *sync.Mutex
	logs [][]byte
}

func (l *logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.m.Lock()
	defer l.m.Unlock()
	for _, b := range l.logs {
		w.Write(b)
	}
}

func (l *logger) write(b []byte) {
	l.m.Lock()
	defer l.m.Unlock()
	l.logs = append(l.logs, b)
}

func (l *logger) Write(b []byte) (int, error) {
	b2 := make([]byte, len(b))
	// Looks like log is reusing its buffer, copy just to be safe
	copy(b2, b)
	go l.write(b2)
	return len(b2), nil
}

type errorHandler func(http.ResponseWriter, *http.Request) error

func (e errorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := e(w, r)

	if err == nil {
		return
	}

	fmt.Fprintf(w, "%s", err)
	log.Println(err)
}

func handlePullRequestUpdated(w http.ResponseWriter, r *http.Request) error {

	repo, ok := r.URL.Query()["repo"]
	if !ok || len(repo) < 1 {
		w.WriteHeader(400)
		return fmt.Errorf(`%s missing "repo" parameter`, r.URL)
	}

	branch, ok := r.URL.Query()["branch"]
	if !ok || len(branch) < 1 {
		w.WriteHeader(400)
		return fmt.Errorf(`%s missing "branch" parameter"`, r.URL)
	}

	status, ok := r.URL.Query()["status"]
	if !ok || len(status) < 1 {
		w.WriteHeader(400)
		return fmt.Errorf(`%s missing "status" parameter`, r.URL)
	}

	switch strings.ToLower(status[0]) {
	case "opened", "reopened":
		log.Printf("creating bot %s %s\n", repo[0], branch[0])
		if err := createBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			return err
		}
		log.Printf("successfully created bot %s %s\n", repo[0], branch[0])
		log.Printf("updating bot %s %s\n", repo[0], branch[0])
		if err := integrateBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			return err
		}
		log.Printf("successfully updated bot %s %s\n", repo[0], branch[0])
	case "closed", "declined":
		log.Printf("deleting bot %s %s\n", repo[0], branch[0])
		if err := deleteBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			return err
		}
		log.Printf("successfully deleted bot %s %s\n", repo[0], branch[0])
	case "rescoped_from":
		log.Printf("updating bot %s %s", repo[0], branch[0])
		if err := integrateBot(repo[0], branch[0]); err != nil {
			w.WriteHeader(500)
			return err
		}
		log.Printf("successfully updated bot %s %s\n", repo[0], branch[0])
	default:
		// nop
		return fmt.Errorf("unknown status: %s\n", status[0])
	}

	w.WriteHeader(201)
	return nil
}

func handleIntegrationUpdated(w http.ResponseWriter, r *http.Request) error {
	success := false
	defer func() {
		if success {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(500)
		}
	}()

	// unlike curl, http doesn't know what to do without a protocol
	if config.BitbucketURL.Scheme == "" {
		config.BitbucketURL.Scheme = "http"
	}

	commit, ok := r.URL.Query()["commit"]
	if !ok || len(commit) < 1 {
		return fmt.Errorf(`%s no "commit" parameter`, r.URL)
	}
	status, ok := r.URL.Query()["status"]
	if !ok || len(status) < 1 {
		return fmt.Errorf(`%s no "status" parameter`, r.URL)
	}
	bot, ok := r.URL.Query()["bot"]
	if !ok || len(bot) < 1 {
		return fmt.Errorf(`%s no "bot" parameter`, r.URL)
	}

	integration, ok := r.URL.Query()["integration"]
	if !ok || len(integration) < 1 {
		return fmt.Errorf(`%s no "integration" parameter`, r.URL)
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
		return fmt.Errorf("handleIntegrationUpdated: %s", err)
	}

	url := &url.URL{}
	*url = *config.BitbucketURL.URL

	url.Path = path.Join(path.Join(config.BitbucketURL.Path, "rest/build-status/1.0/commits/"), commit[0])
	resp, err := bitbucketClient.Post(url.String(), "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("handleIntegrationUpdated: %s", err)
	}
	resp.Body.Close()
	success = true
	return nil
}

func getBots() ([]Bot, error) {
	var botList struct {
		Count   int   `json:"count"`
		Results []Bot `json:"results"`
	}

	resp, err := xcodeClient.Get(config.XcodeURL.String())
	if err != nil {
		return nil, fmt.Errorf("getBot: %s", err)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("getBot: no response from server")
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("getBot: %s", err)
	}

	err = json.Unmarshal(body, &botList)

	if botList.Count == 0 {
		return nil, errors.New("getBot: no bots")
	}

	return botList.Results, nil
}

func getBotsWhere(pred func(*Bot) bool) ([]*Bot, error) {
	bots, err := getBots()
	if err != nil {
		return nil, fmt.Errorf("getBotsWhere: %s", err)
	}
	var result []*Bot
	for i := range bots {
		if pred(&bots[i]) {
			result = append(result, &bots[i])
		}
	}
	return result, nil
}

func getBotNamed(name string) (*Bot, error) {

	bots, err := getBotsWhere(func(b *Bot) bool {
		return strings.ToLower(b.Name) == strings.ToLower(name)
	})

	if err != nil {
		return nil, fmt.Errorf("getBotNamed: %s", err)
	}

	if len(bots) < 1 {
		return nil, fmt.Errorf("getBotNamed %s: no results", name)
	}

	return bots[0], nil
}

func createBot(repo, branch string) error {

	templateBots, err := getBotsWhere(func(b *Bot) bool {
		r, ok := b.Config.envVars["TSUKUROGAMI_REPO_TEMPLATE"].(string)
		if !ok {
			return false
		}
		return strings.ToLower(r) == strings.ToLower(repo)
	})
	if err != nil {
		return fmt.Errorf("createBot %s %s: %s", repo, branch, err)
	}

	if len(templateBots) < 1 {
		return fmt.Errorf("createBot %s %s: no templates for repo", repo, branch)
	}

	for _, templateBot := range templateBots {

		id := templateBot.ID

		prePoke := Trigger{Type: 1, Phase: 1, Name: "Update Status", Body: fmt.Sprintf(pokeStatus, myIP, config.Port, "inprogress")}
		postPoke := Trigger{Type: 1, Phase: 2, Name: "Update Status", Body: fmt.Sprintf(pokeStatus, myIP, config.Port, "${XCS_INTEGRATION_RESULT}")}
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
		templateBot.Config.envVars["TSUKUROGAMI_REPO"] = repo
		templateBot.Config.envVars["TSUKUROGAMI_BRANCH"] = branch
		delete(templateBot.Config.envVars, "TSUKUROGAMI_REPO_TEMPLATE")
		templateBot.Name = templateBot.Name + "." + branch
		templateBot.ID = ""

		templateBot.Config.scheduleType = 3

		newJSON, err := json.Marshal(templateBot)
		if err != nil {
			return fmt.Errorf("createBot %s %s: %s", repo, branch, err)
		}

		resp, err := xcodeClient.Post(fmt.Sprintf("%s/%s/duplicate", config.XcodeURL.String(), id), "application/json", bytes.NewReader(newJSON))
		if err != nil {
			return fmt.Errorf("createBot %s %s: %s", repo, branch, err)
		}

		defer resp.Body.Close()

		if resp.StatusCode != 201 {
			return fmt.Errorf("createBot %s %s: RPC failed (code: %d)", repo, branch, resp.StatusCode)
		}
	}

	return nil
}

func deleteBot(repo, branch string) error {
	bs, err := getBotsWhere(func(b *Bot) bool {
		if r, ok := b.Config.envVars["TSUKUROGAMI_REPO"].(string); !ok || (strings.ToLower(r) != strings.ToLower(repo)) {
			return false
		}
		if br, ok := b.Config.envVars["TSUKUROGAMI_BRANCH"].(string); !ok || (strings.ToLower(br) != strings.ToLower(branch)) {
			return false
		}
		return true
	})

	if err != nil {
		return fmt.Errorf("deleteBot %s %s: %s", repo, branch, err)
	}

	if len(bs) < 1 {
		return fmt.Errorf("deleteBot %s: no bots found", repo)
	}

	for _, b := range bs {
		req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/%s", config.XcodeURL.String(), b.ID), nil)
		if err != nil {
			return fmt.Errorf("deleteBot %s %s: %s", repo, branch, err)
		}

		resp, err := xcodeClient.Do(req)
		if err != nil {
			return fmt.Errorf("deleteBot %s %s: %s", repo, branch, err)
		}

		defer resp.Body.Close()

		if resp.StatusCode != 204 {
			return fmt.Errorf("deleteBot %s %s: RPC failed (code %d)", repo, branch, resp.StatusCode)
		}
	}

	return nil
}

func integrateBot(repo, branch string) error {
	bs, err := getBotsWhere(func(b *Bot) bool {
		if r, ok := b.Config.envVars["TSUKUROGAMI_REPO"].(string); !ok || (strings.ToLower(r) != strings.ToLower(repo)) {
			return false
		}
		if br, ok := b.Config.envVars["TSUKUROGAMI_BRANCH"].(string); !ok || (strings.ToLower(br) != strings.ToLower(branch)) {
			return false
		}
		return true
	})
	if err != nil {
		return fmt.Errorf("integrateBot %s %s: %s", repo, branch, err)
	}

	if len(bs) < 1 {
		return fmt.Errorf("integrateBot %s %s: no bots found", repo, branch)
	}

	for _, b := range bs {
		// downloading sources takes forever with shouldClean: true imo
		resp, err := xcodeClient.Post(fmt.Sprintf("%s/%s/integrations", config.XcodeURL.String(), b.ID), "application/json", strings.NewReader(`{"shouldClean": false}`))
		if err != nil {
			return fmt.Errorf("integrateBot %s %s: %s", repo, branch, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			return fmt.Errorf("integrateBot %s %s: RPC failed (code: %d)", repo, branch, resp.StatusCode)
		}
	}
	return nil
}

func getPreferredIP(hostport string) string {
	// expensive, I know, but seems more accurate than looping through net.Interfaces()
	conn, err := net.Dial("tcp", hostport)
	if err != nil {
		return ""
	}
	addr, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		// wtf
		return ""
	}

	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	if addr.IP.To4() == nil {
		// ipv6 address
		return "[" + host + "]"
	}
	return host
}

func verifyConfig() bool {
	switch {
	case config.XcodeURL.String() == "":
		fallthrough
	case config.BitbucketURL.String() == "":
		fallthrough
	case config.XcodeCredentials == "":
		fallthrough
	case config.BitbucketCredentials == "":
		return false
	default:
		return true
	}

}

var configPath string

func init() {
	// default value
	u, _ := url.Parse("https://localhost:20343/api/bots")
	config.XcodeURL = URL{u}

	flag.Var(&config.XcodeURL, "xcodeURL", "The url of your xcode server")
	flag.Var(&config.BitbucketURL, "bitbucketURL", "The url of your bitbucket server")
	flag.StringVar(&config.XcodeCredentials, "xcodeCredentials", "", "The credentials for your xcode server. username:password")
	flag.StringVar(&config.BitbucketCredentials, "bitbucketCredentials", "", "The credentials for your bitbucket server. username:password")
	flag.IntVar(&config.Port, "port", 4444, "The port to listen on")
	flag.BoolVar(&config.SkipVerify, "skipVerify", true, "Skip certification verification on both servers")

	flag.StringVar(&configPath, "config", "", "If set, the path to the JSON config file used instead of all other command line arguments")
}

func main() {
	flag.Parse()

	if configPath != "" {
		file, err := os.Open(configPath)
		if err != nil {
			log.Fatal(err)
			return
		}
		contents, err := ioutil.ReadAll(file)
		if err != nil {
			log.Fatal(err)
		}
		err = json.Unmarshal(contents, &config)
		if err != nil {
			log.Fatal(err)
		}
	}

	if !verifyConfig() {
		flag.Usage()
		os.Exit(0)
	}

	myIP = getPreferredIP(config.XcodeURL.Host)
	if myIP == "" {
		myIP = "localhost" // not much else we can do
	}

	xcodeClient = &http.Client{Transport: newTransport(config.XcodeCredentials, config.SkipVerify)}
	bitbucketClient = &http.Client{Transport: newTransport(config.BitbucketCredentials, config.SkipVerify)}

	l := &logger{&sync.Mutex{}, [][]byte{}}
	log.SetOutput(io.MultiWriter(os.Stdout, l))

	http.Handle("/pullRequestUpdated", errorHandler(handlePullRequestUpdated))
	http.Handle("/integrationUpdated", errorHandler(handleIntegrationUpdated))
	http.Handle("/logs", l)
	log.Println(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil))
}
