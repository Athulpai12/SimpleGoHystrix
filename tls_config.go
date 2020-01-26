package SimpleGoHystrix




import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"time"
)

func init() {
	// pass
	//If a set of code needs to executed only once then use this function
	// Log as JSON instead of the default ASCII formatter.
	log.SetFormatter(&log.JSONFormatter{})
	log.SetLevel(log.WarnLevel)
}

//This function helps in loading the cert
func loadCert(certPath string, certKey string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, certKey)
	if err != nil {
		params := map[string]interface{}{
			"Cert Path":certPath,
			"Cert Key":certKey,
			"The following error occurred":err,
		}
		log.WithFields(
			log.Fields{
				"The following params were passed ":params,
			})
		return nil, CertError{"loadCert -> Client Failed to load key", err.Error(), params}
	}
	return &cert, err
}

//This function helps in Tls Config
func getTLSConfig(cert *tls.Certificate, caCert *string) (*tls.Config, error) {
	roots := &x509.CertPool{}
	tlsConfig := &tls.Config{}
	if caCert !=nil {
		rootpem, err := ioutil.ReadFile(*caCert)
		if err != nil {
			params := map[string]interface{}{
				"The following Error Occurred":err,
				"Failure point":"getTLSConfig",
			}
			log.WithFields(
				log.Fields{
					"Params":params,
				})
			return nil, IOReadError{"getTLSConfig -> Function Failed in getTLSConfig", err.Error(), params}
		}
		roots = x509.NewCertPool()
		ok := roots.AppendCertsFromPEM(rootpem)
		if !ok {
			log.Println("CA is improper")
			return nil, fmt.Errorf("CA is improper")
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{*cert},
			RootCAs:      nil,
		}
	} else {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{*cert},
			RootCAs:            nil,
		}
	}

	tlsConfig.BuildNameToCertificate()
	return tlsConfig, nil
}

//Return the Tls client
func getTLSClient(config *tls.Config) *http.Client {
	transport := &http.Transport{TLSClientConfig: config}
	client := &http.Client{Transport: transport, Timeout: time.Second * 240}
	return client
}

//load config and returns the Client
//it needs the path of the certificate and key
func getClient(certPath string, certKey string, caCert *string) (*http.Client, error) {
	cert, err := loadCert(certPath, certKey)
	if err != nil {
		return nil,err
	}
	tlsConfig, err := getTLSConfig(cert, caCert)
	if err != nil {
		return nil, errors.Wrap(err, "getClient -> Failed to create certificate")
	}
	client := getTLSClient(tlsConfig)
	return client, nil
}

