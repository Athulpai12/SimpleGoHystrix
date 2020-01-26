package SimpleGoHystrix

import "fmt"

const (
	DEFAULTVALUE = 5
	MAXDURATION  = 300
)

type CertError struct {
	title string
	details string
	params interface{}
}

type IOReadError struct{
	title string
	details string
	params interface{}
}

type NetworkError struct{
	StatusCode int
	title      string
	details    string
	params     interface{}
}

type AppError struct {
	title   string
	details string
}
func (err CertError) Error()string{
	return fmt.Sprintf("title: %d \ndetails :%s\n params :%s",err.title, err.details, err.params)
}

func (err IOReadError)Error()string{
	return fmt.Sprintf("title: %d \ndetails :%s\n params :%s",err.title, err.details, err.params)
}

func (err NetworkError)Error()string{
	return fmt.Sprintf("Status Code: %d \ndetails :%s\n params :%s",err.StatusCode, err.details, err.params)
}

func NewNetworkError(statusCode int, title string, details string, params interface{}) NetworkError{
	return NetworkError{statusCode, title, details, params}
}

func NewCertError(title string, details string, params interface{}) CertError{
	return CertError{title, details, params}
}

func NewIOError(title string, details string, params interface{}) IOReadError{
	return IOReadError{title, details, params}
}