package main

import (
	"fmt"
	"crypto/tls"
	"flag"
	"net/http"
	"encoding/base64"
)

var xcodeURL = flag.String("xcode-url", "", "The url of your xcode server")
var bitbucketURL = flag.String("bitbucket-url", "", "The url of your bitbucket server")
var xcodeCredentials = flag.String("xcode-credentials", "", "The credentials for your xcode server. username:password")
var bitbucketCredentials = flag.String("bitbucket-credentials", "", "The credentials for your bitbucket server. username:password")
var port = flag.Int("port", 4444, "The port to listen on")
var configFilePath = flag.String("config", "", "(Optional) Path to configuration file")
var skipVerify = flag.Bool("skip-verify", true, "Skip certification verification on the xcode server")

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

func main() {
	flag.Parse()
	client = &http.Client{Transport: transport{creds: *xcodeCredentials, Transport: http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *skipVerify}}}}
}
