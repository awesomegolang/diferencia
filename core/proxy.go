package core

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"

	"github.com/lordofthejars/diferencia/difference/json"
	"github.com/lordofthejars/diferencia/exporter"

	"github.com/lordofthejars/diferencia/log"
)

// Difference algorithm
type Difference int

func (difference Difference) String() string {
	names := [...]string{
		"Strict",
		"Subset",
		"Schema"}

	if difference < Strict || difference > Schema {
		return "Unknown"
	}
	return names[difference]
}

//NewDifference creator from String
func NewDifference(difference string) (Difference, error) {

	switch difference {
	case "Strict":
		return Strict, nil
	case "Subset":
		return Subset, nil
	case "Schema":
		return Schema, nil
	}

	return -1, fmt.Errorf("Cannot find %s difference mode", difference)
}

const (
	// Strict mode everything should be exactly the same
	Strict Difference = 0
	// Subset mode that the candidate must be a subset of primary
	Subset Difference = 1
	// Schema mode where the schema must be equal but not the values
	Schema Difference = 2
)

// DiferenciaConfiguration object
type DiferenciaConfiguration struct {
	Port                          int
	Primary, Secondary, Candidate string
	StoreResults                  string
	DifferenceMode                Difference
	NoiseDetection                bool
	AllowUnsafeOperations         bool
}

// IsStoreResultsSet in configuration object
func (conf DiferenciaConfiguration) IsStoreResultsSet() bool {
	return len(conf.StoreResults) > 0
}

// Print configuration
func (conf DiferenciaConfiguration) Print() {
	fmt.Printf("Port: %d\n", conf.Port)
	fmt.Printf("Primary: %s\n", conf.Primary)
	fmt.Printf("Secondary: %s\n", conf.Secondary)
	fmt.Printf("Candidate: %s\n", conf.Candidate)
	fmt.Printf("Store Results: %s\n", conf.StoreResults)
	fmt.Printf("Difference Mode: %s\n", conf.DifferenceMode.String())
	fmt.Printf("Noise Detection: %t\n", conf.NoiseDetection)
	fmt.Printf("Allow Unsafe Operations: %t\n", conf.AllowUnsafeOperations)
}

// HttpClient interface to make requests with changed URL
var HttpClient Client = &HTTPClient{}

var Config *DiferenciaConfiguration

type DiferenciaError struct {
	code    int
	message string
}

func (e *DiferenciaError) Error() string {
	return fmt.Sprintf("with message: %s, and code %d", e.message, e.code)
}

func Diferencia(r *http.Request) (bool, error) {

	if !Config.AllowUnsafeOperations && !isSafeOperation(r.Method) {
		log.Debug("Unsafe operations are not allowed and %s method has been received", r.Method)
		return false, &DiferenciaError{http.StatusMethodNotAllowed, fmt.Sprintf("Unsafe operations are not allowed and %s method has been received", r.Method)}
	}

	log.Debug("URL %s is going to be processed", r.URL.String())

	// TODO it can be parallelized
	// Get request from primary
	primaryFullURL := CreateUrl(*r.URL, Config.Primary)
	log.Debug("Forwarding call to %s", primaryFullURL)
	primaryBodyContent, primaryStatus, _, err := getContent(r, primaryFullURL)
	if err != nil {
		log.Error("Error while connecting to Primary site (%s) with %s", primaryFullURL, err.Error())
		return false, &DiferenciaError{http.StatusServiceUnavailable, fmt.Sprintf("Error while connecting to Primary site (%s) with %s", primaryFullURL, err.Error())}
	}

	// Get candidate
	candidateFullURL := CreateUrl(*r.URL, Config.Candidate)
	log.Debug("Forwarding call to %s", candidateFullURL)
	candidateBodyContent, candidateStatus, _, err := getContent(r, candidateFullURL)
	if err != nil {
		log.Error("Error while connecting to Candidate site (%s) with %s", candidateFullURL, err.Error())
		return false, &DiferenciaError{http.StatusServiceUnavailable, fmt.Sprintf("Error while connecting to Candidate site (%s) with %s", candidateFullURL, err.Error())}
	}

	var result bool

	var secondaryFullURL string
	var secondaryBodyContent []byte
	var secondaryStatus int

	if Config.NoiseDetection {
		// Get secondary to do the noise cancellation
		secondaryFullURL := CreateUrl(*r.URL, Config.Secondary)
		log.Debug("Forwarding call to %s", secondaryFullURL)
		secondaryBodyContent, secondaryStatus, _, err := getContent(r, secondaryFullURL)
		if err != nil {
			log.Error("Error while connecting to Secondary site (%s) with error %s", candidateFullURL, err.Error())
			return false, &DiferenciaError{http.StatusServiceUnavailable, fmt.Sprintf("Error while connecting to Secondary site (%s) with error %s", candidateFullURL, err.Error())}
		}

		// If status code is equal then we detect noise and and remove from primary and candidate
		// What to do in case of two identical status code but no body content (404) might be still valid since you are testing that nothing is there
		if primaryStatus == secondaryStatus {
			noiseOperation := json.NoiseOperation{}
			err := noiseOperation.Detect(primaryBodyContent, secondaryBodyContent)
			if err != nil {
				log.Error("Error detecting noise between %s and %s. (%s)", primaryFullURL, secondaryFullURL, err.Error())
				return false, &DiferenciaError{http.StatusBadRequest, fmt.Sprintf("Error detecting noise between %s and %s. (%s)", primaryFullURL, secondaryFullURL, err.Error())}
			}
			primaryWithoutNoise, candidateWithoutNoise, err := noiseOperation.Remove(primaryBodyContent, candidateBodyContent)

			result = compareResult(candidateWithoutNoise, primaryWithoutNoise, candidateStatus, primaryStatus)
		} else {
			log.Error("Status code between %s(%d) and %s(%d) are different", primaryFullURL, primaryStatus, secondaryFullURL, secondaryStatus)
			return false, &DiferenciaError{http.StatusBadRequest, fmt.Sprintf("Status code between %s(%d) and %s(%d) are different", primaryFullURL, primaryStatus, secondaryFullURL, secondaryStatus)}
		}
	} else {
		// Comparision without noise cancellation
		result = compareResult(candidateBodyContent, primaryBodyContent, candidateStatus, primaryStatus)
	}

	if Config.IsStoreResultsSet() {
		primary := exporter.CreateInteraction(primaryFullURL, primaryBodyContent, primaryStatus)
		candidate := exporter.CreateInteraction(candidateFullURL, candidateBodyContent, candidateStatus)
		var secondary exporter.Interaction

		if Config.NoiseDetection {
			secondary = exporter.CreateInteraction(secondaryFullURL, secondaryBodyContent, secondaryStatus)
		}

		interactions := exporter.CreateInteractions(primary, &secondary, candidate, Config.DifferenceMode.String(), result)

		exporter.ExportToFile(Config.StoreResults, interactions)
	}

	log.Debug("Result of comparing %s and %s is %t", primaryFullURL, candidateFullURL, result)

	return result, nil

}

func compareResult(candidate, primary []byte, candidateStatus, primaryStatus int) bool {
	if primaryStatus == candidateStatus {
		// Comparision between documents without noise
		return json.CompareDocuments(candidate, primary, Config.DifferenceMode.String())
	}
	return false
}

func diferenciaHandler(w http.ResponseWriter, r *http.Request) {

	result, err := Diferencia(r)
	if err != nil {
		if de, ok := err.(*DiferenciaError); ok {
			w.WriteHeader(de.code)
			fmt.Fprintf(w, de.message)
		}

		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, err.Error())
	}

	if result {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusPreconditionFailed)
	}

}

// StartProxy server
func StartProxy(configuration *DiferenciaConfiguration) {
	Config = configuration
	Config.Print()

	// Matches everything
	http.HandleFunc("/", diferenciaHandler)
	log.Error("Error starting proxy: %s", http.ListenAndServe(":"+strconv.Itoa(Config.Port), nil))
}

func isSafeOperation(method string) bool {
	return method == http.MethodGet || method == http.MethodOptions || method == http.MethodHead
}

func getContent(r *http.Request, url string) ([]byte, int, http.Header, error) {
	resp, err := HttpClient.MakeRequest(r, url)

	if err != nil {
		// In case of error in service we should add as metrics as well or assume that the service itself would communicate to metrics?
		return make([]byte, 0), 0, nil, err
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()

	return bodyBytes, resp.StatusCode, resp.Header, err

}
