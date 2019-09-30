package SimpleGoHystrix

import (
	"errors"
	"fmt"
	"io"
	"math"
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

func ExponentialBackoff(i int) time.Duration {
	return time.Duration(1<<uint(i)) * time.Second
}

func ExponentialBackoffJitter(i int) time.Duration {
	return jitter(1 << uint(i))
}

func defaultBackoff(i int) time.Duration {
	return DEFAULTVALUE * time.Second
}

func LinearBackoff(i int) time.Duration {
	return time.Duration(i*DEFAULTVALUE) * time.Second
}

func LinearBackoffJitter(i int) time.Duration {
	return jitter(uint(i * DEFAULTVALUE))
}

func jitter(i uint) time.Duration {
	return time.Duration(random.Intn(int(math.Min(MAXDURATION, float64(i))))) * time.Second
}

type backoffAlgo func(int) time.Duration

type logginghook func(ErrLog)

func (c *Client) log(e ErrLog) {
	if c.keeplog {
		//locking is required to avoid race conditions
		c.Lock()
		c.errData = append(c.errData, e)
		c.Unlock()
	}
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

func retry(ctx context.Context, wg *sync.WaitGroup, c *Client, req *Request, multiplexChan chan<- result, closerResultChan <-chan bool, retry int, backoff backoffAlgo) {
	fmt.Println("in go routine")
	wg.Add(1)
	defer func(){
		fmt.Println("here for closure")
		wg.Done()
	}()
	var resp *http.Response
	var err error
	// retries for with backoff strategy
	for attempt := 0; attempt < retry; attempt++ {
		//Reset body to the beginning of response as the body is drained every time
		fmt.Println(retry, "lets see attempts", attempt)
		select {
		case <-ctx.Done():
			fmt.Println("forced cancel")
			err := errors.New("forced Cancel error")
			multiplexChan <- result{nil, err}
			return
		case <-closerResultChan:
			return
		default:
			fmt.Println("out of heere")
		}
		fmt.Println("in here n out")
		resp, err = c.client.Do(req.Request)
		fmt.Println("receied terp value")
		errLog := ErrLog{
			method:  req.Request.Method,
			url:     req.Request.URL.RequestURI(),
			body:    req.body,
			request: 0,
			retry:   retry,
			attempt: attempt,
			err:     err,
		}
		c.log(errLog)
		fmt.Println(resp,err)
		if err == nil && resp.StatusCode < 500 {

			multiplexChan <- result{resp: resp, err: nil}
			fmt.Println("out of routine  and in m ultiplex")
			return
		}

		select {
		// After will fail if backoff return zero (to prevent that error 1 extra millisecond is added)
		case <-time.After(backoff(attempt) + 1*time.Millisecond):
		//canceling a go-routine
		case <-ctx.Done():
			err := errors.New("forced Cancel error")
			multiplexChan <- result{nil, err}
			return
		case <-closerResultChan:
			return
		}

	}
	//exp := utils.RaiseNetworkError("The go routine failed with the following error", err, nil)
	multiplexChan <- result{resp: resp, err: err}
	return
}

func multiplex(ctx context.Context, multiplexChan <-chan result, resultChan chan<- result, finishChan <-chan bool) {
	//This is done to prevent memory leaks in go routine
	var firstData = false
	var res result
	for {
		select {
		case res = <-multiplexChan:
			if !firstData {
				firstData = true
				resultChan <- res
				close(resultChan)
			} else {
				response := res.resp
				if response != nil {
					response.Body.Close()
				}
			}
		case <-finishChan:
			fmt.Println("finally finished")
			return


		}
	}
	fmt.Println("Ended this routine")
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