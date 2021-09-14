package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	cliRecon "github.com/kyma-incubator/reconciler/internal/cli/reconciler"
	cliTest "github.com/kyma-incubator/reconciler/internal/cli/test"
	"github.com/kyma-incubator/reconciler/pkg/logger"
	"github.com/kyma-incubator/reconciler/pkg/reconciler"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes/adapter"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes/progress"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/service"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/workspace"
	"github.com/kyma-incubator/reconciler/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"io/ioutil"
	clientgo "k8s.io/client-go/kubernetes"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	urlCallbackHTTPBin = "https://httpbin.org/post"
	urlCallbackMock    = "http://localhost:11111/callback"

	urlReconcilerRun = "http://localhost:9999/v1/run"

	componentReconcilerName = "unittest"
	workerTimeout           = 1 * time.Minute
	serverPort              = 9999

	componentName      = "component-1"
	componentNamespace = "inttest-comprecon"
	componentVersion   = "0.0.0"
	componentPod       = "dummy-pod"
)

type testCase struct {
	name               string
	model              *reconciler.Reconciliation
	expectedHTTPCode   int
	expectedResponse   interface{}
	verifyCallbacksFct func(t *testing.T, callbacks []*reconciler.CallbackMessage)
	verifyResponseFct  func(*testing.T, interface{})
}

func TestReconciler(t *testing.T) {
	test.IntegrationTest(t)

	setGlobalWorkspaceFactory(t)
	kubeClient := newKubeClient(t)
	recon := newComponentReconciler(t) //register and configure a new component reconciler

	//cleanup old pods after and before test execution
	cleanup := service.NewTestCleanup(recon, kubeClient)
	cleanup.RemoveKymaComponent(t, componentVersion, componentName, componentNamespace)       //cleanup before tests are executed
	defer cleanup.RemoveKymaComponent(t, componentVersion, componentName, componentNamespace) //cleanup after tests are finished

	//create runtime context which is cancelled at the end of the test
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(1 * time.Second) //give some time for graceful shutdown
	}()
	startReconciler(ctx, t)

	runTestCases(t, kubeClient)
}

func setGlobalWorkspaceFactory(t *testing.T) {
	//use ./test folder as workspace
	wsf, err := workspace.NewFactory("test", logger.NewOptionalLogger(true))
	require.NoError(t, err)
	require.NoError(t, service.UseGlobalWorkspaceFactory(wsf))
}

func newKubeClient(t *testing.T) kubernetes.Client {
	//create kubeClient (e.g. needed to verify reconciliation results)
	kubeClient, err := adapter.NewKubernetesClient(test.ReadKubeconfig(t), logger.NewOptionalLogger(true), nil)
	require.NoError(t, err)
	return kubeClient
}

func newComponentReconciler(t *testing.T) *service.ComponentReconciler {
	//create reconciler
	recon, err := service.NewComponentReconciler(componentReconcilerName) //register brand new component reconciler
	require.NoError(t, err)
	//configure reconciler
	require.NoError(t, recon.WithDependencies("abc", "xyz").Debug())
	return recon
}

func startReconciler(ctx context.Context, t *testing.T) {
	go func() {
		o := cliRecon.NewOptions(cliTest.NewTestOptions(t))
		o.ServerConfig.Port = serverPort
		o.WorkerConfig.Timeout = workerTimeout
		o.HeartbeatSenderConfig.Interval = 1 * time.Second
		o.ProgressTrackerConfig.Interval = 1 * time.Second
		o.RetryConfig.RetryDelay = 2 * time.Second
		o.RetryConfig.MaxRetries = 3

		workerPool, err := StartComponentReconciler(ctx, o, componentReconcilerName)
		require.NoError(t, err)

		require.NoError(t, StartWebserver(ctx, o, workerPool))
	}()
	cliTest.WaitForTCPSocket(t, "localhost", serverPort, 5*time.Second)
}

func post(t *testing.T, testCase testCase) interface{} {
	jsonPayload, err := json.Marshal(testCase.model)
	require.NoError(t, err)

	//nolint:gosec //in test cases is a dynamic URL acceptable
	resp, err := http.Post(urlReconcilerRun, "application/json",
		bytes.NewBuffer(jsonPayload))
	require.NoError(t, err)
	require.Equal(t, testCase.expectedHTTPCode, resp.StatusCode)

	//read body
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()
	body, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)

	t.Logf("Body received: %s", string(body))

	require.NoError(t, json.Unmarshal(body, testCase.expectedResponse))
	return testCase.expectedResponse
}

func runTestCases(t *testing.T, kubeClient kubernetes.Client) {
	//execute test cases
	testCases := []testCase{
		{
			name: "Missing dependencies",
			model: &reconciler.Reconciliation{
				ComponentsReady: []string{"abc", "def"},
				Component:       componentName,
				Namespace:       componentNamespace,
				Version:         "1.2.3",
				Profile:         "unittest",
				Configuration:   nil,
				Kubeconfig:      "xyz",
				InstallCRD:      false,
				CorrelationID:   "test-correlation-id",
			},
			expectedHTTPCode: http.StatusPreconditionRequired,
			expectedResponse: &reconciler.HTTPMissingDependenciesResponse{},
			verifyResponseFct: func(t *testing.T, responseModel interface{}) {
				resp := responseModel.(*reconciler.HTTPMissingDependenciesResponse)
				require.Equal(t, []string{"abc", "xyz"}, resp.Dependencies.Required)
				require.Equal(t, []string{"xyz"}, resp.Dependencies.Missing)
			},
		},
		{
			name: "Invalid request: mandatory fields missing",
			model: &reconciler.Reconciliation{
				ComponentsReady: []string{"abc", "xyz"},
				Component:       componentName,
				Namespace:       componentNamespace,
				Version:         "1.2.3",
				Profile:         "",
				Configuration:   nil,
				Kubeconfig:      "",
				InstallCRD:      false,
				CorrelationID:   "test-correlation-id",
				CallbackFunc:    nil,
			},
			expectedHTTPCode: http.StatusBadRequest,
			expectedResponse: &reconciler.HTTPErrorResponse{},
			verifyResponseFct: func(t *testing.T, responseModel interface{}) {
				require.IsType(t, &reconciler.HTTPErrorResponse{}, responseModel)
			},
		},
		{
			name: "Install component from scratch",
			model: &reconciler.Reconciliation{
				ComponentsReady: []string{"abc", "xyz"},
				Component:       componentName,
				Namespace:       componentNamespace,
				Version:         componentVersion,
				Profile:         "",
				Configuration:   nil,
				Kubeconfig:      test.ReadKubeconfig(t),
				InstallCRD:      false,
				CorrelationID:   "test-correlation-id",
			},
			expectedHTTPCode: http.StatusOK,
			expectedResponse: &reconciler.HTTPReconciliationResponse{},
			verifyResponseFct: func(t *testing.T, i interface{}) {
				expectRunningPod(t, kubeClient) //wait until pod is ready
			},
		},
		{
			name: "Try to apply impossible change: add container to running pod",
			model: &reconciler.Reconciliation{
				ComponentsReady: []string{"abc", "xyz"},
				Component:       componentName,
				Namespace:       componentNamespace,
				Version:         componentVersion,
				Profile:         "",
				Configuration: []reconciler.Configuration{
					{
						Key:   "initContainer",
						Value: true,
					},
				},
				Kubeconfig:    test.ReadKubeconfig(t),
				InstallCRD:    false,
				CorrelationID: "test-correlation-id",
			},
			expectedHTTPCode: http.StatusOK,
			expectedResponse: &reconciler.HTTPReconciliationResponse{},
			verifyCallbacksFct: func(t *testing.T, callbacks []*reconciler.CallbackMessage) {
				require.Equal(t, reconciler.Error, callbacks[len(callbacks)-1].Status) // last status should be error
			},
		},

		//TODO: non-reachable cluster, insufficient permissions on cluster, defective helm-chart, non-started MS service
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, newTestFct(testCase))
	}
}

func newProgressTracker(t *testing.T, clientSet clientgo.Interface) *progress.Tracker {
	prog, err := progress.NewProgressTracker(clientSet, logger.NewOptionalLogger(true), progress.Config{
		Interval: 1 * time.Second,
	})
	require.NoError(t, err)
	return prog
}

//newTestFct is required to make the linter happy ;)
func newTestFct(testCase testCase) func(t *testing.T) {
	return func(t *testing.T) {
		//inject mock-url into model if required
		if testCase.verifyCallbacksFct == nil {
			testCase.model.CallbackURL = urlCallbackHTTPBin
		} else {
			testCase.model.CallbackURL = urlCallbackMock
		}

		respModel := post(t, testCase)
		if testCase.verifyResponseFct != nil {
			testCase.verifyResponseFct(t, respModel)
		}

		if testCase.verifyCallbacksFct != nil {
			timer := time.NewTimer(workerTimeout)
			var callbackC = newCallbackMock(t)
			var received []*reconciler.CallbackMessage
		Loop:
			for {
				select {
				case callback := <-callbackC:
					received = append(received, callback)
					if finalStatus(callback.Status) {
						break Loop
					}
				case <-timer.C:
					t.Logf("Timeout reached for retrieving callbacks")
					break Loop
				}
			}
			testCase.verifyCallbacksFct(t, received)
		}
	}
}

func newCallbackMock(t *testing.T) chan *reconciler.CallbackMessage {
	callbackC := make(chan *reconciler.CallbackMessage, 100) //don't block

	go func(t *testing.T, cbChannel chan *reconciler.CallbackMessage) {
		router := mux.NewRouter()

		router.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			body, err := ioutil.ReadAll(r.Body)
			defer require.NoError(t, r.Body.Close())
			require.NoError(t, err, "Failed to read HTTP callback message")

			t.Logf("Received HTTP callback: %s", string(body))

			callbackData := make(map[string]interface{})
			require.NoError(t, json.Unmarshal(body, &callbackData))

			status, err := newStatus(fmt.Sprintf("%s", callbackData["status"]))
			require.NoError(t, err)

			cbChannel <- &reconciler.CallbackMessage{
				Status: status,
				Error:  errors.New(fmt.Sprintf("%s", callbackData["error"])),
			}
		})
		go func() {
			require.NoError(t, http.ListenAndServe(":11111", router))
		}()
	}(t, callbackC)

	return callbackC
}

func expectRunningPod(t *testing.T, kubeClient kubernetes.Client) {
	clientSet, err := kubeClient.Clientset()
	require.NoError(t, err)

	watchable, err := progress.NewWatchableResource("pod")
	require.NoError(t, err)

	t.Logf("Waiting for pod '%s' to reach READY state", componentPod)
	prog := newProgressTracker(t, clientSet)
	prog.AddResource(watchable, componentNamespace, componentPod)
	require.NoError(t, prog.Watch(context.TODO(), progress.ReadyState))
	t.Logf("Pod '%s' reached READY state", componentPod)
}

func finalStatus(status reconciler.Status) bool {
	return status == reconciler.Error || status == reconciler.Success
}

func newStatus(status string) (reconciler.Status, error) {
	switch strings.ToLower(status) {
	case string(reconciler.NotStarted):
		return reconciler.NotStarted, nil
	case string(reconciler.Failed):
		return reconciler.Failed, nil
	case string(reconciler.Error):
		return reconciler.Error, nil
	case string(reconciler.Running):
		return reconciler.Running, nil
	case string(reconciler.Success):
		return reconciler.Success, nil
	default:
		return "", fmt.Errorf("status '%s' not found", status)
	}
}