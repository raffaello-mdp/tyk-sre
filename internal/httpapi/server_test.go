package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/raffaello-mdp/tyk-sre/internal/hardening"
)

// fakeHardener records the namespaces and dry-run flag it receives.
type fakeHardener struct {
	called     bool
	dryRun     bool
	namespaces []string
	results    []hardening.NamespaceResult
}

func (f *fakeHardener) Harden(_ context.Context, namespaces []string, dryRun bool) []hardening.NamespaceResult {
	f.called = true
	f.dryRun = dryRun
	f.namespaces = namespaces
	return f.results
}

func post(handler http.Handler, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)))
	return recorder
}

func TestHardenRejectsInvalidRequests(t *testing.T) {
	tests := map[string]struct {
		body string
		want int
	}{
		"malformed JSON":     {body: `{"namespaces":`, want: http.StatusBadRequest},
		"empty body":         {body: ``, want: http.StatusBadRequest},
		"no namespaces":      {body: `{"namespaces":[]}`, want: http.StatusBadRequest},
		"missing field":      {body: `{}`, want: http.StatusBadRequest},
		"unknown field":      {body: `{"namespaces":["a"],"namespace":"b"}`, want: http.StatusBadRequest},
		"invalid name":       {body: `{"namespaces":["Not_Valid"]}`, want: http.StatusBadRequest},
		"empty name":         {body: `{"namespaces":[""]}`, want: http.StatusBadRequest},
		"trailing content":   {body: `{"namespaces":["a"]}{"namespaces":["b"]}`, want: http.StatusBadRequest},
		"body over the size": {body: `{"namespaces":["` + strings.Repeat("a", maxBodyBytes) + `"]}`, want: http.StatusRequestEntityTooLarge},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			hardener := &fakeHardener{}

			response := post(NewHandler(hardener), "/hardening", test.body)

			if response.Code != test.want {
				t.Errorf("status = %d, want %d: %s", response.Code, test.want, response.Body)
			}
			if hardener.called {
				t.Error("an invalid request reached the hardener")
			}
		})
	}
}

// Tricky: a request meant as a preview must reach the hardener as a dry run,
// however the parameter is written, or it would change live workloads.
func TestHardenPassesDryRun(t *testing.T) {
	tests := map[string]bool{
		"/hardening":              false,
		"/hardening?dryRun=false": false,
		"/hardening?dryRun=true":  true,
		"/hardening?dryRun=1":     true,
		"/hardening?dryRun":       true,
	}
	for target, want := range tests {
		t.Run(target, func(t *testing.T) {
			hardener := &fakeHardener{}

			response := post(NewHandler(hardener), target, `{"namespaces":["tenant-a"]}`)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body)
			}
			if hardener.dryRun != want {
				t.Errorf("dry run = %v, want %v", hardener.dryRun, want)
			}
			var body hardenResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if body.DryRun != want {
				t.Errorf("response dryRun = %v, want %v", body.DryRun, want)
			}
		})
	}
}

func TestHardenRejectsInvalidDryRun(t *testing.T) {
	hardener := &fakeHardener{}

	response := post(NewHandler(hardener), "/hardening?dryRun=maybe", `{"namespaces":["tenant-a"]}`)

	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if hardener.called {
		t.Error("a request with an invalid dryRun reached the hardener")
	}
}

func TestHardenPassesUniqueNamespacesInOrder(t *testing.T) {
	hardener := &fakeHardener{}

	response := post(NewHandler(hardener), "/hardening", `{"namespaces":["b","a","b"]}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body)
	}
	if want := []string{"b", "a"}; !slices.Equal(hardener.namespaces, want) {
		t.Errorf("namespaces = %v, want %v", hardener.namespaces, want)
	}
}

func TestHardenCountsFailures(t *testing.T) {
	failedWorkload := hardening.WorkloadResult{Kind: "Deployment", Name: "web", Status: hardening.StatusFailed}
	patchedWorkload := hardening.WorkloadResult{Kind: "Deployment", Name: "api", Status: hardening.StatusPatched}

	tests := map[string]struct {
		results []hardening.NamespaceResult
		want    int
	}{
		"all succeeded": {
			results: []hardening.NamespaceResult{{
				Namespace: "a",
				Status:    hardening.StatusProcessed,
				Workloads: []hardening.WorkloadResult{patchedWorkload},
			}},
		},
		"skipped is not a failure": {
			results: []hardening.NamespaceResult{{Namespace: "kube-system", Status: hardening.StatusSkipped}},
		},
		"namespace failed": {
			results: []hardening.NamespaceResult{{Namespace: "a", Status: hardening.StatusFailed}},
			want:    1,
		},
		"one workload failed among others": {
			results: []hardening.NamespaceResult{{
				Namespace: "a",
				Status:    hardening.StatusProcessed,
				Workloads: []hardening.WorkloadResult{failedWorkload, patchedWorkload},
			}},
			want: 1,
		},
		"failures add up across namespaces": {
			results: []hardening.NamespaceResult{
				{Namespace: "a", Status: hardening.StatusFailed, Workloads: []hardening.WorkloadResult{failedWorkload}},
				{Namespace: "b", Status: hardening.StatusProcessed, Workloads: []hardening.WorkloadResult{failedWorkload}},
			},
			want: 3,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			response := post(NewHandler(&fakeHardener{results: test.results}), "/hardening", `{"namespaces":["a"]}`)

			if response.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
			}
			var body hardenResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if body.Failed != test.want {
				t.Errorf("failed = %d, want %d", body.Failed, test.want)
			}
			if len(body.Namespaces) != len(test.results) {
				t.Errorf("response lists %d namespaces, want %d", len(body.Namespaces), len(test.results))
			}
		})
	}
}

func TestHardenOnlyAcceptsPost(t *testing.T) {
	recorder := httptest.NewRecorder()

	NewHandler(&fakeHardener{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/hardening", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}
