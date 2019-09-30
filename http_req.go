package SimpleGoHystrix

import (
	"io"
	"math/rand"
	"net/http"
	"runtime"
	"sync"
	"context"
	"time"
)

type Request struct {
	// ReadSeeker inplace of default Reader because we need to seek back to zero when a post call fails
	body io.ReadSeeker
	*http.Request
}

//this is for caching the transport layer data
type cacheTransport struct {
	data map[interface{}]interface{}
	//To avoid race condition
	mu                sync.RWMutex
	originalTransport http.RoundTripper
}

type result struct {
	//The response object given by go routines
	resp *http.Response
	err  error
}

type ErrLog struct {
	method  string
	url     string
	body    io.ReadSeeker
	request int
	retry   int
	attempt int
	err     error
}

const (
	DEFAULTVALUE = 5
	MAXDURATION  = 300
)

//Random variable for introducing jitter
var random *rand.Rand

func init() {
	//This is introducing a seed in PRNG

	random = rand.New(rand.NewSource(time.Now().UnixNano()))
}


type Client struct {
	//Using the default http client
	client *http.Client
	// the features available for client
	transport     http.RoundTripper
	jar           http.CookieJar
	checkRedirect func(req *http.Request, via []*http.Request) error
	timeout       time.Duration

	//New features available on the client
	concurrency int
	retry       int

	backOff backoffAlgo

	//Log hook for the current every request
	logHook logginghook

	// keep log for hook
	keeplog bool

	//Error log for each client
	errData []ErrLog

	//for making the library thread safe
	wg *sync.WaitGroup

	sync.Mutex
}

func (c *Client) Do(ctx context.Context, req *Request) (*http.Response, error) {
	resultChan := make(chan result, 1)
	multiplexChan := make(chan result)
	closeResultChan := make(chan bool)
	finishChan := make(chan bool)
	//dummy channel to close all the routines running
	concurrency := c.concurrency
	if req.Method != "GET" {
		concurrency = 1
	}
	httpClient := &http.Client{
		Transport:     c.transport,
		CheckRedirect: c.checkRedirect,
		Jar:           c.jar,
		Timeout:       c.timeout,
	}
	//concurrency = 5
	c.retry = 4
	c.client = httpClient
	cleanUpWg := &sync.WaitGroup{}
	cleanUpWg.Add(1)
	defer cleanUpWg.Done()
	for ret := 0; ret < concurrency; ret++ {
		go retry(ctx, cleanUpWg, c, req, multiplexChan, closeResultChan, c.retry, c.backOff)
		fmt.Println(" while concurrency No of go routine ",runtime.NumGoroutine())
	}

	go multiplex(ctx, multiplexChan, resultChan, finishChan)
	fmt.Println("while multiplex No of go routine ",runtime.NumGoroutine())
	//cleaning up the left over request
	go func() {
		cleanUpWg.Wait()
		close(finishChan)
		resp := <- resultChan
		if resp.resp != nil{
			resp.resp.Body.Close()
		}
		fmt.Println("finished finish chan")

	}()
	fmt.Println("FTER CLEAN UP No of go routine ",runtime.NumGoroutine())
	fmt.Println()
	var output result
	select {
	case output = <-resultChan:
	case <-ctx.Done():
	}
	fmt.Println("CLOSED CLOSE RESULT CHAN")
	close(closeResultChan)
	fmt.Println("hello world")
	return output.resp, output.err
}