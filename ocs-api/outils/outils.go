package outils

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Utilities for ocs-api

const (
	HTTPRequestTimeoutS        = 30
	MaxHTTPIdleConnections     = 20
	HTTPIdleConnectionTimeoutS = 120
)

var IsVerbose bool
var HttpClient *http.Client

// A "subclass" of error that also contains the http code that should be sent to the client
type HttpError struct {
	Code int
	Err  error
}

func NewHttpError(code int, errStr string, args ...interface{}) *HttpError {
	return &HttpError{Code: code, Err: errors.New(fmt.Sprintf(errStr, args...))}
}

func (e *HttpError) Error() string {
	return e.Err.Error()
}

// Verify that the request content type is json
func IsValidPostJson(r *http.Request) *HttpError {
	val, ok := r.Header["Content-Type"]

	if !ok || len(val) == 0 || val[0] != "application/json" {
		return NewHttpError(http.StatusBadRequest, "Error: content-type must be application/json)")
	}
	return nil
}

// Parse the json string into the given struct
func ParseJsonString(jsonBytes []byte, bodyStruct interface{}) *HttpError {
	err := json.Unmarshal(jsonBytes, bodyStruct)
	if err != nil {
		return NewHttpError(http.StatusBadRequest, "Error parsing request body json bytes: "+err.Error())
	}
	return nil
}

// Parse the request json body into the given struct
func ReadJsonBody(r *http.Request, bodyStruct interface{}) *HttpError {
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(bodyStruct)
	if err != nil {
		return NewHttpError(http.StatusBadRequest, "Error parsing request body: "+err.Error())
	}
	return nil
}

// Respond to the client with this code and body struct
func WriteJsonResponse(httpCode int, w http.ResponseWriter, bodyStruct interface{}) {
	dataJson, err := json.Marshal(bodyStruct)
	if err != nil {
		http.Error(w, "Internal Server Error (could not encode json response)", http.StatusInternalServerError)
		return
	}
	WriteResponse(httpCode, w, dataJson)
}

// Respond to the client with this code and body bytes
func WriteResponse(httpCode int, w http.ResponseWriter, bodyBytes []byte) {
	w.WriteHeader(httpCode) // seems like this has to be before writing the body
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write(bodyBytes)
	if err != nil {
		Error(err.Error())
	}
}

// Generate a random node token
func GenerateNodeToken() (string, *HttpError) {
	bytes := make([]byte, 22) // 44 hex chars
	_, err := rand.Read(bytes)
	if err != nil {
		return "", NewHttpError(http.StatusInternalServerError, "Error creating random bytes for node token: "+err.Error())
	}
	return hex.EncodeToString(bytes), nil
}

// Convert a space-separated string into a null separated string (with extra null at end)
func MakeExecCmd(execString string) string {
	returnStr := ""
	for _, w := range strings.Fields(execString) {
		returnStr += w + "\x00"
	}
	return returnStr + "\x00"
}

// Initialize the verbose setting
func SetVerbose() {
	v := GetEnvVarWithDefault("VERBOSE", "false")
	if v == "1" || strings.ToLower(v) == "true" {
		IsVerbose = true
	} else {
		IsVerbose = false
	}
}

// Print error msg to stderr
func Verbose(msg string, args ...interface{}) {
	if !IsVerbose {
		return
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Printf("Verbose: "+msg, args...)
}

// Print error msg to stderr
func Error(msg string, args ...interface{}) {
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	l := log.New(os.Stderr, "", 0)
	l.Printf("Error: "+msg, args...)
}

// Print error msg to stderr and exit with the specified code
func Fatal(exitCode int, msg string, args ...interface{}) {
	Error(msg, args...)
	os.Exit(exitCode)
}

// Get this environment variable or use this default
func GetEnvVarWithDefault(envVarName, defaultValue string) string {
	envVarValue := os.Getenv(envVarName)
	if envVarValue == "" {
		return defaultValue
	}
	return envVarValue
}

// Returns true if this env var is set
func IsEnvVarSet(envVarName string) bool {
	return os.Getenv(envVarName) != ""
}

// Returns true if this file or dir exists
func PathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Download a file from a web site (that doesn't require authentication)
func DownloadFile(url, fileName string, perm os.FileMode) error {
	// Set up the request
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	newFile, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer newFile.Close()
	if err := newFile.Chmod(perm); err != nil {
		return err
	}

	// Write the request body to the file. This streams the content straight from the request to the file, so works for large files
	_, err = io.Copy(newFile, resp.Body)
	return err
}

// Copy a file
func CopyFile(fromFileName, toFileName string, perm os.FileMode) *HttpError {
	var content []byte
	var err error
	if content, err = ioutil.ReadFile(fromFileName); err != nil {
		return NewHttpError(http.StatusInternalServerError, "could not read "+fromFileName+": "+err.Error())
	}
	if err = ioutil.WriteFile(toFileName, content, perm); err != nil {
		return NewHttpError(http.StatusInternalServerError, "could not write "+toFileName+": "+err.Error())
	}
	return nil
}

// Verify the request credentials with the exchange. Returns true/false or error
func ExchangeAuthenticate(r *http.Request, currentExchangeUrl, currentOrgId, certificatePath string) (bool, *HttpError) {
	orgAndUser, pwOrKey, ok := r.BasicAuth()
	if !ok {
		return false, nil
	}
	parts := strings.Split(orgAndUser, "/")
	if len(parts) != 2 {
		return false, nil
	}
	orgId := parts[0]
	user := parts[1]

	var certPath string
	if !PathExists(certificatePath) {
		certPath = ""
	} else {
		certPath = certificatePath
	}

	var url, method string
	var goodStatusCode int
	if orgAndUser == "root/root" {
		// Special case of exchange root user: in this case it is ok for the creds org to be different from the request org
		//To do this do GET /orgs/{orgid}/users
		method = http.MethodGet
		url = fmt.Sprintf("%v/orgs/%v/users", currentExchangeUrl, currentOrgId)
		goodStatusCode = http.StatusOK
	} else {
		// Non-root creds: Invoke exchange to confirm the client has user creds in the org of the creds, and that it is the same org this service is configured for.
		// To do this do POST /orgs/{orgid}/users/{username}/confirm. Note: this is not supported for the exchange root creds.
		if orgId != currentOrgId {
			return false, NewHttpError(http.StatusUnauthorized, "the org id of the credentials ("+orgId+") does not match the org id the SDO owner service is configured for ("+currentOrgId+")")
		}
		method = http.MethodPost
		url = fmt.Sprintf("%v/orgs/%v/users/%v/confirm", currentExchangeUrl, orgId, user)
		goodStatusCode = http.StatusCreated
	}
	apiMsg := fmt.Sprintf("%v %v", method, url)
	Verbose("confirming credentials via %s", apiMsg)

	// Create an outgoing HTTP request for the exchange.
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return false, NewHttpError(http.StatusInternalServerError, "unable to create HTTP request for %s, error: %v", apiMsg, err)
	}

	// Add the basic auth header so that the exchange will authenticate.
	req.SetBasicAuth(orgAndUser, pwOrKey)
	req.Header.Add("Accept", "application/json")

	// Send the request to verify the user.
	httpClient, httpErr := GetHTTPClient(certPath)
	if httpErr != nil {
		return false, httpErr
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, NewHttpError(http.StatusInternalServerError, "unable to send HTTP request for %s, error: %v", apiMsg, err)
	} else if resp.StatusCode == goodStatusCode {
		return true, nil
	} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, nil
	} else {
		return false, NewHttpError(resp.StatusCode, "unexpected http status code received from %s: %d", apiMsg, resp.StatusCode)
	}

}
func GetHTTPClient(certPath string) (*http.Client, *HttpError) {
	// Try to reuse the 1 global client
	if HttpClient == nil {
		var httpErr *HttpError
		if HttpClient, httpErr = NewHTTPClient(certPath); httpErr != nil {
			return nil, httpErr
		}
	}
	return HttpClient, nil
}

// Common function for getting an HTTP client connection object.
func NewHTTPClient(certPath string) (*http.Client, *HttpError) {
	httpClient := &http.Client{
		// remember that this timeout is for the whole request, including
		// body reading. This means that you must set the timeout according
		// to the total payload size you expect
		Timeout: time.Second * time.Duration(HTTPRequestTimeoutS),
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   20 * time.Second,
				KeepAlive: 60 * time.Second,
			}).Dial,
			TLSHandshakeTimeout:   20 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ExpectContinueTimeout: 8 * time.Second,
			MaxIdleConns:          MaxHTTPIdleConnections,
			IdleConnTimeout:       HTTPIdleConnectionTimeoutS * time.Second,
			//TLSClientConfig: &tls.Config{ InsecureSkipVerify: skipSSL }, // <- this is set by TrustIcpCert()
		},
	}
	if httpErr := TrustIcpCert(httpClient.Transport.(*http.Transport), certPath); httpErr != nil {
		return nil, httpErr
	}

	return httpClient, nil
}

/* TrustIcpCert adds the icp cert file to be trusted (if exists) in calls made by the given http client. 3 cases:
1. no cert is needed because a CA-trusted cert is being used, or the svr uses http
2. a self-signed cert is being used, but they told us to connect insecurely
3. a non-blank certPath is specified that we will use
*/
func TrustIcpCert(transport *http.Transport, certPath string) *HttpError {
	// Case 2:
	if os.Getenv("HZN_SSL_SKIP_VERIFY") != "" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		return nil
	}
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: false}

	// Case 1:
	if certPath == "" {
		return nil
	}

	// Case 3:
	icpCert, err := ioutil.ReadFile(filepath.Clean(certPath))
	if err != nil {
		NewHttpError(http.StatusInternalServerError, "Encountered error reading ICP cert file %v: %v", certPath, err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(icpCert)

	transport.TLSClientConfig.RootCAs = caCertPool
	return nil
}
