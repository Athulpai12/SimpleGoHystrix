package SimpleGoHystrix

import (
	"bytes"
	"encoding/json"
	"github.com/pkg/errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"net/url"
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

//sanitizes the result and returns and appropriate response and error status
func sanitizeResult(res *result, statusCode map[int]bool) result {
	if res.err == nil && res.resp == nil {
		res.err = NewNetworkError(-1, "Error occurred cause the request was cancelled", "", nil)
		return *res
	}
	if res.resp == nil {
		return *res
	}
	if _, ok := statusCode[res.resp.StatusCode]; !ok {
		res.resp.Body.Close()
		res.err = NewNetworkError(res.resp.StatusCode, "Returned status code does not match", "", nil)
		return *res
	}
	return *res
}

type ErrLog struct {
	responseCode int
	method       string
	url          string
	body         io.ReadSeeker
	request      int
	retry        int
	attempt      int
	err          string
	response     string
}


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
		c.ErrData = append(c.ErrData, e)
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
	Concurrency int
	Retry       int

	BackOff backoffAlgo

	//Log hook for the current every request
	logHook logginghook

	// keep log for hook
	keeplog bool

	//Error log for each client
	ErrData []ErrLog

	//for making the library thread safe
	wg *sync.WaitGroup

	sync.Mutex

	getStatusCodes map[int]bool

	postStatusCodes map[int]bool

	putStatusCodes map[int]bool

	certPath string

	certKey string
}

func retry(ctx context.Context, wg *sync.WaitGroup, c *Client, req *Request, multiplexChan chan<- result,
	closerResultChan <-chan bool, retry int, backoff backoffAlgo, statusCode map[int]bool, index int) {
	wg.Add(1)
	defer func(){
		wg.Done()
	}()
	var resp *http.Response
	var err error
	var res result
	// retries for with backoff strategy
	for attempt := 0; attempt < retry; attempt++ {
		//Reset body to the beginning of response as the body is drained every time
		select {
		case <-ctx.Done():
			err := errors.New("forced Cancel error")
			multiplexChan <- result{nil, err}
			return
		case <-closerResultChan:
			return
		default:
		}
		if req.Request.Body != nil {
			req.body.Seek(0, 0)
		}
		resp, err = c.client.Do(req.Request)
		res = result{resp, err}
		res = sanitizeResult(&res, statusCode)
		errMsg := ""
		respCode := 0
		if res.err != nil {
			errMsg = res.err.Error()
		} else {
			respCode = res.resp.StatusCode
		}

		errLog := ErrLog{
			responseCode: respCode,
			method:       req.Request.Method,
			url:          req.Request.URL.RequestURI(),
			body:         req.body,
			request:      0,
			retry:        index,
			attempt:      attempt,
			err:          errMsg,
		}
		c.log(errLog)
		if res.err == nil {
			multiplexChan <- res
			return
		}

		if attempt == retry-1 {
			break
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
	res = sanitizeResult(&res, statusCode)
	multiplexChan <- res
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
	concurrency := c.Concurrency
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
	c.Retry = 4
	c.client = httpClient
	cleanUpWg := &sync.WaitGroup{}
	cleanUpWg.Add(1)
	defer cleanUpWg.Done()
	var statusCode map[int]bool
	switch req.Request.Method {
	case "GET":
		statusCode = c.getStatusCodes
	case "POST":
		statusCode = c.postStatusCodes
	case "PUT":
		statusCode = c.putStatusCodes
	}
	for ret := 0; ret < concurrency; ret++ {
		go retry(ctx, cleanUpWg, c, req, multiplexChan, closeResultChan, c.Retry, c.BackOff, statusCode, ret)
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

func (c *Client) UpdateTransportLayer(transport http.RoundTripper) {
	c.transport = transport
}

func (c *Client) UpdateJar(jar http.CookieJar) {
	c.jar = jar
}

func (c *Client) UpdateTimeout(timeout time.Duration) {
	c.timeout = timeout
}

func (c *Client) UpdateClient(certPath string, certKey string){
	c.certPath = certPath
	c.certKey = certKey
}

func (c *Client) UpdatehttpClient(client *http.Client) {
	c.transport = client.Transport
	c.checkRedirect = client.CheckRedirect
	c.jar = client.Jar
	c.timeout = client.Timeout
}

func EncodeUrl(baseurl string, params map[string]string) (string, error) {
	baseUrl, err := url.Parse(baseurl)
	if err != nil {
		return "",err
	}

	// Add a Path Segment (Path segment is automatically escaped)
	//baseUrl.Path += "path with?reserved characters"

	// Prepare Query Parameters
	urlParams := url.Values{}
	for key,value :=range params {
		urlParams.Add(key,value)
	}

	// Add Query Parameters to the URL
	baseUrl.RawQuery = urlParams.Encode() // Escape Query Parameters
	return baseUrl.String(),nil
}

func addHeader(header map[string]string, req *http.Request) {
	if header != nil {
		for key, value := range header {
			req.Header.Add(key, value)
		}
	}
}

func (c *Client) Get(ctx context.Context, url string, params map[string]string, header map[string]string) (*http.Response, error) {
	var err error
	if params != nil {
		url, err = EncodeUrl(url, params)
	}
	req, err := NewRequest(url, "GET", nil, header, "")
	if err!=nil{
		return nil, errors.Wrap(err,"Client Get()  -> Get request failed for "+url)
	}
	addHeader(header, req.Request)
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "Client Get() -> Get request failed for "+url)
	}
	return resp, err
}

//overriding http.NewRequest
func NewRequest(url, method string, body io.ReadSeeker, headers map[string]string, bodyType string) (*Request, error) {
	var rcBody io.ReadCloser
	if body != nil {
		rcBody = ioutil.NopCloser(body)
	}
	httpReq, err := http.NewRequest(method, url, rcBody)
	if err != nil {
		return nil, err
	}
	return &Request{body, httpReq}, err
}

func getJson(body interface{})(io.ReadSeeker, error){
	bytesRepresentation, err := json.Marshal(body)
	if err != nil {
		return nil, errors.Wrap(err,"getJson -> Failed to convert data to json")
	}
	data := bytes.NewReader(bytesRepresentation)
	return data,nil

}
func (c *Client) PostWithContext(ctx context.Context, url string, bodyType string, body interface{}, header map[string]string) (*http.Response, error) {
	data,err := getJson(body)
	if err!=nil{
		return nil, errors.Wrap(err, "PostWithContext() -> failed to create a new request")
	}
	req, err := NewRequest(url, "POST", data, header, "")
	if err != nil {
		return nil, errors.Wrap(err, "PostWithContext -> failed to create a new request")
	}
	addHeader(header, req.Request)
	req.Request.Header.Add("Content-Type", bodyType)
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "PostWithContext -> failed to create a new request")
	}
	return resp, err
}

func (c *Client) Post(url string, bodyType string, body interface{}, header map[string]string) (string, error){
	data,err := getJson(body)
	if err!=nil{
		return "", errors.Wrap(err, "Post() -> failed to create a new request")
	}
	req, err := NewRequest(url, "POST", data, header, bodyType)
	if err != nil {
		return "", errors.Wrap(err, "Client Post() -> failed to create a new request")
	}
	addHeader(header, req.Request)
	req.Request.Header.Add("Content-Type", bodyType)
	ctx := context.Background()
	resp, err := c.Do(ctx, req)
	if err != nil {
		return "", errors.Wrap(err, "Post() -> failed to create a new request")
	}
	strData, err := GetData(resp, nil)
	if err!=nil{
		return "", errors.Wrap(err, "Post() -> failed to create a new request")
	}
	return strData,err
}

func (c *Client)Put(url string, bodyType string, body interface{}, header map[string]string)(string, error){
	// complete this
	data, err := getJson(body)
	if err!=nil{
		return "", errors.Wrap(err, "Post() -> failed to create a new request")
	}
	req, err := NewRequest(url, "PUT", data, header, bodyType)
	if err != nil {
		return "", errors.Wrap(err, "Client Post() -> failed to create a new request")
	}
	addHeader(header, req.Request)
	req.Request.Header.Add("Content-Type", bodyType)
	ctx := context.Background()
	resp, err := c.Do(ctx, req)
	if err != nil {
		return "", errors.Wrap(err, "Post() -> failed to create a new request")
	}
	strData, err := GetData(resp, nil)
	if err!=nil{
		return "", errors.Wrap(err, "Post() -> failed to create a new request")
	}
	return strData,err
}

func GetData(resp *http.Response, context interface{}) (string, error) {
	if resp == nil {
		return "", errors.Wrap(nil, "Post() -> failed to create a new request")
	}
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", NewIOError("Error occurred"+
			" while reading body", err.Error(), context)
	}
	resp.Body.Close()
	bodyString := string(bodyBytes)
	return bodyString, nil
}

func Get(url string, params map[string]string, header map[string]string) (string, error) {
	client, err := NewHttpClient()
	if err != nil {
		return "", errors.Wrap(err, "Get() -> Failed to create a get request")
	}
	ctx := context.Background()
	resp, err := client.Get(ctx, url, params, header)
	if err == nil {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", errors.Wrap(err, "Get() -> Failed to create a get request")
		}
		bodyString := string(bodyBytes)
		return bodyString, nil
	}
	if resp != nil {
		resp.Body.Close()
	}
	return "", err
}

func Post(url string, bodyType string, body interface{}, header map[string]string) (string, error) {
	client, err := NewHttpClient()
	if err != nil {
		return "", errors.Wrap(err, "Post -> Failed to create a Post request")
	}
	ctx := context.Background()
	resp, err := client.PostWithContext(ctx, url, bodyType, body, header)
	if err != nil {
		return "", errors.Wrap(err, "Post -> Failed to create a Post request")

	}
	data, err := GetData(resp, client.ErrData)
	if err!=nil{
		return "", errors.Wrap(err, "Post -> Failed to create a Post request")
	}
	return data, nil
}

//return the client with default settinns
func NewHttpClient() (*Client, error) {
	client := &Client{}
	client.Concurrency = 1
	client.Retry = 5
	client.timeout = 300 * time.Second
	client.BackOff = ExponentialBackoffJitter
	client.keeplog = true
	client.getStatusCodes = map[int]bool{
		200: true,
	}
	client.postStatusCodes = map[int]bool{
		201: true,
		200: true,
	}
	client.putStatusCodes = map[int]bool{
		201:true,
	}
	return client, nil
}

func (c *Client)UpdateGetStatusCode(statusCode map[int]bool){
	c.getStatusCodes = statusCode
}

func (c *Client) UpdatePostStatusCode(statusCode map[int]bool)  {
	c.postStatusCodes = statusCode
}

func (c *Client)UpdatePutStatusCode(statusCode map[int]bool){
	c.putStatusCodes = statusCode
}

//returns a new tls client
func NewTLSHttpClient(certPath *string, key *string, caCert *string) (*Client, error) {
	client, err := NewHttpClient()
	if certPath ==nil || key==nil{
		return client,err
	}
	tlsClient, err := getClient(*certPath, *key, caCert)
	if err != nil {
		return nil, errors.Wrap(err, "NewTLSHttpClient() -> Failed to create a new TLS client")
	}

	if err != nil {
		return nil, errors.Wrap(err, "NewTLSHttpClient() -> Failed to create a new TLS client")
	}
	client.UpdatehttpClient(tlsClient)
	client.UpdateClient(*certPath, *key)
	return client, err

}