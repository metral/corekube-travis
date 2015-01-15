package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"time"

	"github.com/metral/corekube-travis/rax"
	"github.com/metral/corekube-travis/util"
	"github.com/metral/goutils"
	"github.com/metral/overlord/lib"
)

var (
	templateFilepath = flag.String("templateFilePath", "", "Filepath of corekube-heat.yaml")
)

func getStackDetails(result *util.CreateStackResult) util.StackDetails {
	var details util.StackDetails
	url := (*result).Stack.Links[0].Href
	token := rax.IdentitySetup()

	headers := map[string]string{
		"X-Auth-Token": token.ID,
		"Content-Type": "application/json",
	}

	p := goutils.HttpRequestParams{
		HttpRequestType: "GET",
		Url:             url,
		Headers:         headers,
	}

	statusCode, bodyBytes := goutils.HttpCreateRequest(p)

	switch statusCode {
	case 200:
		err := json.Unmarshal(bodyBytes, &details)
		goutils.CheckForErrors(goutils.ErrorParams{Err: err, CallerNum: 1})
	}

	return details
}

func watchStackCreation(result *util.CreateStackResult) util.StackDetails {
	sleepDuration := 10 // seconds
	var details util.StackDetails

watchLoop:
	for {
		details = getStackDetails(result)
		log.Printf("Stack Status: %s", details.Stack.StackStatus)

		switch details.Stack.StackStatus {
		case "CREATE_IN_PROGRESS":
			time.Sleep(time.Duration(sleepDuration) * time.Second)
		case "CREATE_COMPLETE":
			break watchLoop
		default:
			log.Printf("Stack Status: %s", details.Stack.StackStatus)
			log.Printf("Stack Status: %s", details.Stack.StackStatusReason)
			deleteStack(result.Stack.Links[0].Href)
			log.Fatal()
		}
	}

	return details
}

func startStackTimeout(heatTimeout int, result *util.CreateStackResult) util.StackDetails {
	chan1 := make(chan util.StackDetails, 1)
	go func() {
		stackDetails := watchStackCreation(result)
		chan1 <- stackDetails
	}()

	select {
	case result := <-chan1:
		return result
	case <-time.After(time.Duration(heatTimeout) * time.Minute):
		msg := fmt.Sprintf("Stack create timed out after %d mins", heatTimeout)
		deleteStack(result.Stack.Links[0].Href)
		log.Fatal(msg)
	}
	return *new(util.StackDetails)
}

func createStackReq(template, token, keyName string) (int, []byte) {
	timeout := int(10)
	params := map[string]string{
		"key-name": keyName,
	}
	disableRollback := bool(false)

	timestamp := int32(time.Now().Unix())
	templateName := fmt.Sprintf("corekube-travis-%d", timestamp)

	log.Printf("Started creating stack: %s", templateName)

	s := &util.HeatStack{
		Name:            templateName,
		Template:        template,
		Params:          params,
		Timeout:         timeout,
		DisableRollback: disableRollback,
	}
	jsonByte, _ := json.Marshal(s)

	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Auth-Token": token,
	}

	urlStr := fmt.Sprintf("%s/stacks", os.Getenv("TRAVIS_OS_HEAT_URL"))

	h := goutils.HttpRequestParams{
		HttpRequestType: "POST",
		Url:             urlStr,
		Data:            jsonByte,
		Headers:         headers,
	}

	statusCode, bodyBytes := goutils.HttpCreateRequest(h)
	return statusCode, bodyBytes
}

func createStack(templateFile, keyName string) util.CreateStackResult {
	readfile, _ := ioutil.ReadFile(templateFile)
	template := string(readfile)
	var result util.CreateStackResult

	token := rax.IdentitySetup()

	statusCode, bodyBytes := createStackReq(template, token.ID, keyName)

	switch statusCode {
	case 201:
		err := json.Unmarshal(bodyBytes, &result)
		goutils.CheckForErrors(goutils.ErrorParams{Err: err, CallerNum: 1})
	}
	return result
}

func getFunctionName(i interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
}

func overlayNetworksCountTest(details *util.StackDetails) string {
	d := *details
	msg := ""
	sleepDuration := 10 //seconds

	for {
		msg = "overlayNetworksCountTest: "

		overlordIP := extractOverlordIP(d)
		masterCount, _ := strconv.Atoi(
			d.Stack.Parameters["kubernetes-master-count"].(string))
		minionCount, _ := strconv.Atoi(
			d.Stack.Parameters["kubernetes-minion-count"].(string))
		expectedCount := masterCount + minionCount

		var overlayResult lib.Result
		path := fmt.Sprintf("%s/keys/coreos.com/network/subnets",
			lib.ETCD_API_VERSION)
		url := fmt.Sprintf("http://%s:%s/%s",
			overlordIP, lib.ETCD_CLIENT_PORT, path)

		token := rax.IdentitySetup()

		headers := map[string]string{
			"X-Auth-Token": token.ID,
			"Content-Type": "application/json",
		}

		p := goutils.HttpRequestParams{
			HttpRequestType: "GET",
			Url:             url,
			Headers:         headers,
		}

		_, jsonResponse := goutils.HttpCreateRequest(p)
		err := json.Unmarshal(jsonResponse, &overlayResult)
		goutils.CheckForErrors(goutils.ErrorParams{Err: err, CallerNum: 2})

		overlayNetworksCount := len(overlayResult.Node.Nodes)

		if overlayNetworksCount == expectedCount {
			return "Passed"
		}

		msg += fmt.Sprintf("ExpectedCount: %d, OverlayNetworkCount: %d",
			expectedCount, overlayNetworksCount)
		log.Printf(msg)
		time.Sleep(time.Duration(sleepDuration) * time.Second)
	}

	return "Failed"
}

func startTestTimeout(timeout int, details *util.StackDetails, f func(*util.StackDetails) string) {
	chan1 := make(chan string, 1)
	go func() {
		result := f(details)
		chan1 <- result
	}()

	select {
	case result := <-chan1:
		msg := fmt.Sprintf("%s %s.", getFunctionName(f), result)
		log.Printf(msg)
	case <-time.After(time.Duration(timeout) * time.Minute):
		msg := fmt.Sprintf("%s Failed: timed out after %d mins.",
			getFunctionName(f), timeout)
		log.Fatal(msg)
	}
}

func runTests(details *util.StackDetails) {
	startTestTimeout(1, details, overlayNetworksCountTest)
}

func extractOverlordIP(details util.StackDetails) string {
	overlordIP := ""

	for _, i := range details.Stack.Outputs {
		if i.OutputKey == "overlord_ip" {
			overlordIP = i.OutputValue.(string)
		}
	}

	return overlordIP
}

func deleteStack(stackUrl string) {
	token := rax.IdentitySetup()

	headers := map[string]string{
		"X-Auth-Token": token.ID,
		"Content-Type": "application/json",
	}

	p := goutils.HttpRequestParams{
		HttpRequestType: "DELETE",
		Url:             stackUrl,
		Headers:         headers,
	}

	statusCode, _ := goutils.HttpCreateRequest(p)

	switch statusCode {
	case 204:
		log.Printf("Delete stack requested.")
	}

}

func main() {
	flag.Parse()

	heatTimeout := 10 // minutes
	templateFile := *templateFilepath
	keyName := os.Getenv("TRAVIS_OS_KEYPAIR")

	result := createStack(templateFile, keyName)
	stackDetails := startStackTimeout(heatTimeout, &result)
	runTests(&stackDetails)
	//deleteStack(stackDetails.Stack.Links[0].Href)
}
