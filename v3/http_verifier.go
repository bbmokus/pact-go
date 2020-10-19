package v3

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pact-foundation/pact-go/proxy"
)

type HTTPVerifier struct {
	// ClientTimeout specifies how long to wait for Pact CLI to start
	// Can be increased to reduce likelihood of intermittent failure
	// Defaults to 10s
	ClientTimeout time.Duration
}

func (v *HTTPVerifier) validateConfig() error {
	if v.ClientTimeout == 0 {
		v.ClientTimeout = 10 * time.Second
	}

	return nil
}

// VerifyProviderRaw reads the provided pact files and runs verification against
// a running Provider API, providing raw response from the Verification process.
//
// Order of events: BeforeEach, stateHandlers, requestFilter(pre <execute provider> post), AfterEach
func (v *HTTPVerifier) verifyProviderRaw(request VerifyRequest, writer outputWriter) error {
	err := v.validateConfig()

	if err != nil {
		return err
	}

	u, err := url.Parse(request.ProviderBaseURL)

	m := []proxy.Middleware{}

	if request.BeforeEach != nil {
		m = append(m, BeforeEachMiddleware(request.BeforeEach))
	}

	if request.AfterEach != nil {
		m = append(m, AfterEachMiddleware(request.AfterEach))
	}

	if len(request.StateHandlers) > 0 {
		m = append(m, stateHandlerMiddleware(request.StateHandlers))
	}

	if request.RequestFilter != nil {
		m = append(m, request.RequestFilter)
	}

	// Configure HTTP Verification Proxy
	opts := proxy.Options{
		TargetAddress:             fmt.Sprintf("%s:%s", u.Hostname(), u.Port()),
		TargetScheme:              u.Scheme,
		TargetPath:                u.Path,
		Middleware:                m,
		InternalRequestPathPrefix: providerStatesSetupPath,
		CustomTLSConfig:           request.CustomTLSConfig,
	}

	// Starts the message wrapper API with hooks back to the state handlers
	// This maps the 'description' field of a message pact, to a function handler
	// that will implement the message producer. This function must return an object and optionally
	// and error. The object will be marshalled to JSON for comparison.
	port, err := proxy.HTTPReverseProxy(opts)

	// Backwards compatibility, setup old provider states URL if given
	// Otherwise point to proxy
	setupURL := request.ProviderStatesSetupURL
	if request.ProviderStatesSetupURL == "" && len(request.StateHandlers) > 0 {
		setupURL = fmt.Sprintf("http://localhost:%d%s", port, providerStatesSetupPath)
	}

	// Construct verifier request
	verificationRequest := VerifyRequest{
		ProviderBaseURL:            fmt.Sprintf("http://localhost:%d", port),
		PactURLs:                   request.PactURLs,
		PactFiles:                  request.PactFiles,
		PactDirs:                   request.PactDirs,
		BrokerURL:                  request.BrokerURL,
		Tags:                       request.Tags,
		BrokerUsername:             request.BrokerUsername,
		BrokerPassword:             request.BrokerPassword,
		BrokerToken:                request.BrokerToken,
		PublishVerificationResults: request.PublishVerificationResults,
		ProviderVersion:            request.ProviderVersion,
		Provider:                   request.Provider,
		ProviderStatesSetupURL:     setupURL,
		ProviderTags:               request.ProviderTags,
		// CustomProviderHeaders:      request.CustomProviderHeaders,
		// ConsumerVersionSelectors:   request.ConsumerVersionSelectors,
		// EnablePending:              request.EnablePending,
		FailIfNoPactsFound: request.FailIfNoPactsFound,
		// IncludeWIPPactsSince:       request.IncludeWIPPactsSince,
	}

	portErr := waitForPort(port, "tcp", "localhost", v.ClientTimeout,
		fmt.Sprintf(`Timed out waiting for http verification proxy on port %d - check for errors`, port))

	if portErr != nil {
		log.Fatal("Error:", err)
		return portErr
	}

	log.Println("[DEBUG] pact provider verification")

	return verificationRequest.verify(writer)
}

// VerifyProvider accepts an instance of `*testing.T`
// running the provider verification with granular test reporting and
// automatic failure reporting for nice, simple tests.
func (v *HTTPVerifier) VerifyProvider(t *testing.T, request VerifyRequest) error {
	err := v.verifyProviderRaw(request, t)

	// TODO: granular test reporting
	// runTestCases(t, res)

	t.Run("Provider pact verification", func(t *testing.T) {
		if err != nil {
			t.Error(err)
		}
	})

	return err
}

// BeforeEachMiddleware is invoked before any other, only on the __setup
// request (to avoid duplication)
func BeforeEachMiddleware(BeforeEach Hook) proxy.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == providerStatesSetupPath {

				log.Println("[DEBUG] executing before hook")
				err := BeforeEach()

				if err != nil {
					log.Println("[ERROR] error executing before hook:", err)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AfterEachMiddleware is invoked after any other, and is the last
// function to be called prior to returning to the test suite. It is
// therefore not invoked on __setup
func AfterEachMiddleware(AfterEach Hook) proxy.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			if r.URL.Path != providerStatesSetupPath {
				log.Println("[DEBUG] executing after hook")
				err := AfterEach()

				if err != nil {
					log.Println("[ERROR] error executing after hook:", err)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}
		})
	}
}

// {"action":"teardown","id":"foo","state":"User foo exists"}
type stateHandlerAction struct {
	Action string `json:"action"`
	State  string `json:"state"`
	// Params map[string]interface{}
}

// stateHandlerMiddleware responds to the various states that are
// given during provider verification
//
// statehandler accepts a state object from the verifier and executes
// any state handlers associated with the provider.
// It will not execute further middleware if it is the designted "state" request
func stateHandlerMiddleware(stateHandlers StateHandlers) proxy.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == providerStatesSetupPath {
				log.Println("[INFO] executing state handler middleware")
				var state stateHandlerAction
				buf := new(strings.Builder)
				io.Copy(buf, r.Body)
				log.Println("[TRACE] state handler received input", buf.String())

				err := json.Unmarshal([]byte(buf.String()), &state)
				log.Println("[TRACE] state handler received input", state)

				if err != nil {
					log.Println("[ERROR] unable to decode incoming state change payload", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				// Setup any provider state
				sf, stateFound := stateHandlers[state.State]

				if !stateFound {
					log.Printf("[WARN] no state handler found for state: %v", state.State)
				} else {
					// Execute state handler
					if err := sf(ProviderStateV3{Name: state.State}); err != nil {
						log.Printf("[ERROR] state handler for '%v' errored: %v", state.State, err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}

				w.WriteHeader(http.StatusOK)
				return
			}

			log.Println("[TRACE] skipping state handler for request", r.RequestURI)

			// Pass through to application
			next.ServeHTTP(w, r)
		})
	}
}

const providerStatesSetupPath = "/__setup/"

// Use this to wait for a port to be running prior
// to running tests.
var waitForPort = func(port int, network string, address string, timeoutDuration time.Duration, message string) error {
	log.Println("[DEBUG] waiting for port", port, "to become available")
	timeout := time.After(timeoutDuration)

	for {
		select {
		case <-timeout:
			log.Printf("[ERROR] Expected server to start < %s. %s", timeoutDuration, message)
			return fmt.Errorf("Expected server to start < %s. %s", timeoutDuration, message)
		case <-time.After(50 * time.Millisecond):
			_, err := net.Dial(network, fmt.Sprintf("%s:%d", address, port))
			if err == nil {
				return nil
			}
		}
	}
}