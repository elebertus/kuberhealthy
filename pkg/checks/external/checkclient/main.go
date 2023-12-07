// Package checkclient implements a client for reporting the state of an
// externally spawned checker pod to Kuberhealthy.  The URL that reports are
// sent to are pulled from the environment variables of the pod because
// Kuberhealthy sets them all all external checkers when they are spawned.
package checkclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/cenkalti/backoff"

	"github.com/kuberhealthy/kuberhealthy/v2/pkg/checks/external"
	"github.com/kuberhealthy/kuberhealthy/v2/pkg/checks/external/status"
)

var (
	// Debug can be used to enable output logging from the checkClient
	Debug bool
)

// Use exponential backoff for retries
const maxElapsedTime = time.Second * 30

// ReportSuccess reports a successful check run to the Kuberhealthy service. We
// do not return an error here because failures will cause the managing
// instance of Kuberhealthy to time out and show an error.
func ReportSuccess() error {
	writeLog("DEBUG: Reporting SUCCESS")

	// make a new report without errors
	newReport := status.NewReport([]string{})

	// send the payload
	return sendReport(newReport)
}

// ReportFailure reports that the external checker has found problems.  You may
// pass a slice of error message strings that will surface in the Kuberhealthy
// status page for more context on the failure.  We do not return an error here
// because the managing instance of Kuberhealthy for this check will detect the
// failure to report-in and raise an error upstream.
func ReportFailure(errorMessages []string) error {
	writeLog("DEBUG: Reporting FAILURE")

	// make a new report without errors
	newReport := status.NewReport(errorMessages)

	// send it
	return sendReport(newReport)
}

// writeLog writes a log entry if debugging is enabled
func writeLog(i ...interface{}) {
	if Debug {
		log.Println("checkClient:", fmt.Sprint(i...))
	}
}

// sendReport marshals the report and sends it to the kuberhealthy endpoint
// as shown in the environment variables.
func sendReport(s status.Report) error {

	writeLog("DEBUG: Sending report with error length of:", len(s.Errors))
	writeLog("DEBUG: Sending report with ok state of:", s.OK)

	exponentialBackOff := backoff.NewExponentialBackOff()
	exponentialBackOff.MaxElapsedTime = maxElapsedTime

	// fetch the server url outside of the backoff.Retry function body
	// so it can be used later in logging as well.
	url, err := getKuberhealthyURL()
	if err != nil {
		return fmt.Errorf("failed to fetch the kuberhealthy url: %w", err)
	}
	writeLog("INFO: Using kuberhealthy reporting URL: ", url)

	client := &http.Client{}

	// send to the server
	var statusCode int
	backoffErr := backoff.Retry(func() error {
		// If we don't craft a new request on succesive retry the request will not get sent
		req, err := newKuberhealthyReportRequest(s, url)
		if err != nil {
			writeLog("Error generating kuberhealthy request with body ", s)
			return fmt.Errorf("error generating kuberhealthy request with body %v", s)
		}

		resp, reqErr := client.Do(req)
		statusCode = resp.StatusCode
		// retry on any errors
		if reqErr != nil {
			return reqErr
		}
		// retry on status codes that do not return a 400
		switch {
		case statusCode == http.StatusBadRequest:
			writeLog("ERROR: got a fatal status code from kuberhealthy: ", statusCode)
			// Break from the backoff.Retry loop since 400 indicates we're sending a junk
			// request
			return backoff.Permanent(fmt.Errorf("fatal status code from kuberhealthy status reporting url: [%d] \"%s\" body: %v", resp.StatusCode, resp.Status, s))
		case statusCode != http.StatusOK && statusCode != http.StatusCreated:
			writeLog("ERROR: got a bad status code from kuberhealthy: ", statusCode)
			return fmt.Errorf("ERROR: got a bad status code from kuberhealthy: %d", statusCode)
		default:
			// something undexpected has happened, since there is no context for this error
			// we will not mark it as fatal.
			writeLog("INFO: No error found in POST ", resp.Status)
			return reqErr
		}
	}, exponentialBackOff)
	if backoffErr != nil {
		writeLog("ERROR: got an error sending POST to kuberhealthy: ", backoffErr)
		return fmt.Errorf("bad POST request to kuberhealthy status reporting url: %w", backoffErr)
	}

	writeLog("INFO: Got a good http return status code from kuberhealthy URL: ", url, statusCode)

	return backoffErr
}

// newKuberhealthyReportRequest return a request object with the appropriate headers set
func newKuberhealthyReportRequest(s status.Report, u string) (*http.Request, error) {
	var req *http.Request

	// fetch the kh run UUID
	uuid, err := getKuberhealthyRunUUID()
	if err != nil {
		return req, fmt.Errorf("failed to fetch the kuberhealthy run uuid: %w", err)
	}
	writeLog("INFO: Using kuberhealthy run UUID: ", uuid)

	// marshal the request body
	b, err := json.Marshal(s)
	if err != nil {
		writeLog("ERROR: Failed to marshal status JSON:", err)
		return req, fmt.Errorf("error mashaling status report json: %w", err)
	}

	// create the Kuberhealthy post request with the kh-run-uuid header
	req, err = http.NewRequest(http.MethodPost, u, bytes.NewBuffer(b))
	if err != nil {
		return req, fmt.Errorf("error creating http request: %w", err)
	}
	req.Header.Set("kh-run-uuid", uuid)
	req.Header.Set("Content-Type", "application/json")

	return req, err
}

// getKuberhealthyURL fetches the URL that we need to send our external checker
// status report to from the environment variables
func getKuberhealthyURL() (string, error) {

	reportingURL := os.Getenv(external.KHReportingURL)

	// check the length of the reporting url to make sure we pulled one properly
	if len(reportingURL) < 1 {
		writeLog("ERROR: kuberhealthy reporting URL from environment variable", external.KHReportingURL, "was blank")
		return "", fmt.Errorf("fetched %s environment variable but it was blank", external.KHReportingURL)
	}

	return reportingURL, nil
}

// getKuberhealthyRunUUID fetches the kuberheathy checker pod run UUID to send to our external checker
// status to report to from the environment variable
func getKuberhealthyRunUUID() (string, error) {

	khRunUUID := os.Getenv(external.KHRunUUID)

	// check the length of the UUID to make sure we pulled one properly
	if len(khRunUUID) < 1 {
		writeLog("ERROR: kuberhealthy run UUID from environment variable", external.KHRunUUID, "was blank")
		return "", fmt.Errorf("fetched %s environment variable but it was blank", external.KHRunUUID)
	}

	return khRunUUID, nil
}

// GetDeadline fetches the KH_CHECK_RUN_DEADLINE environment variable and returns it.
// Checks are given up to the deadline to complete their check runs.
func GetDeadline() (time.Time, error) {
	unixDeadline := os.Getenv(external.KHDeadline)

	if len(unixDeadline) < 1 {
		writeLog("ERROR: kuberhealthy check deadline from environment variable", external.KHDeadline, "was blank")
		return time.Time{}, fmt.Errorf("fetched %s environment variable but it was blank", external.KHDeadline)
	}

	unixDeadlineInt, err := strconv.Atoi(unixDeadline)
	if err != nil {
		writeLog("ERROR: unable to parse", external.KHDeadline+": "+err.Error())
		return time.Time{}, fmt.Errorf("unable to parse %s: %s", external.KHDeadline, err.Error())
	}

	return time.Unix(int64(unixDeadlineInt), 0), nil
}
