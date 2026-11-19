// Package httpapi exposes the hardening operation over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/raffaello-mdp/tyk-sre/internal/hardening"
)

const (
	// maxBodyBytes is far more than any legitimate list of namespaces.
	maxBodyBytes = 1 << 20
	// requestTimeout bounds a request if the API server stops responding.
	requestTimeout = 2 * time.Minute
	// shutdownTimeout fits within the terminationGracePeriodSeconds of the Deployment.
	shutdownTimeout = 30 * time.Second
)

// Hardener applies the hardening to the given namespaces, or with dryRun only
// computes it.
type Hardener interface {
	Harden(ctx context.Context, namespaces []string, dryRun bool) []hardening.NamespaceResult
}

// hardenRequest is the body of POST /hardening.
type hardenRequest struct {
	Namespaces []string `json:"namespaces"`
}

// hardenResponse is the body returned once the namespaces have been processed.
type hardenResponse struct {
	// DryRun is true when the changes were computed but not persisted.
	DryRun bool `json:"dryRun"`
	// Failed counts the namespaces and workloads that could not be processed.
	Failed     int                         `json:"failed"`
	Namespaces []hardening.NamespaceResult `json:"namespaces"`
}

// errorResponse is the body returned when a request is rejected.
type errorResponse struct {
	Error string `json:"error"`
}

// NewHandler returns the routes of the API.
func NewHandler(hardener Hardener) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /hardening", hardenHandler(hardener))
	return mux
}

// NewServer returns the API server, to be run by the manager.
func NewServer(addr string, hardener Hardener) *manager.Server {
	timeout := shutdownTimeout
	return &manager.Server{
		Name: "hardening-api",
		Server: &http.Server{
			Addr:              addr,
			Handler:           NewHandler(hardener),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		ShutdownTimeout: &timeout,
	}
}

// hardenHandler parses the request and hardens the namespaces it names. It answers
// 200 once the request has run, even if parts of it failed: the body says what
// happened to each workload, and its failed count tells whether all went well.
func hardenHandler(hardener Hardener) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		isDryRun, err := parseDryRun(r)
		if err != nil {
			writeJSON(r.Context(), w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		namespaces, err := parseNamespaces(w, r)
		if err != nil {
			status := http.StatusBadRequest
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeJSON(r.Context(), w, status, errorResponse{Error: err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		results := hardener.Harden(ctx, namespaces, isDryRun)

		failed := 0
		for _, result := range results {
			failed += result.Failures()
		}
		writeJSON(r.Context(), w, http.StatusOK, hardenResponse{DryRun: isDryRun, Failed: failed, Namespaces: results})
	}
}

// parseDryRun reads the dryRun query parameter. A bare ?dryRun counts as true,
// so that a request meant as a preview is never applied by mistake.
func parseDryRun(r *http.Request) (bool, error) {
	query := r.URL.Query()
	if !query.Has("dryRun") {
		return false, nil
	}
	value := query.Get("dryRun")
	if value == "" {
		return true, nil
	}
	dryRun, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid dryRun %q: must be true or false", value)
	}
	return dryRun, nil
}

// parseNamespaces reads and validates the request body, dropping repeated names.
func parseNamespaces(w http.ResponseWriter, r *http.Request) ([]string, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	var request hardenRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid request body: expected a single JSON object")
	}
	if len(request.Namespaces) == 0 {
		return nil, errors.New("namespaces must list at least one namespace")
	}

	namespaces := make([]string, 0, len(request.Namespaces))
	for _, namespace := range request.Namespaces {
		if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
			return nil, fmt.Errorf("invalid namespace %q: %s", namespace, strings.Join(problems, ", "))
		}
		if !slices.Contains(namespaces, namespace) {
			namespaces = append(namespaces, namespace)
		}
	}
	return namespaces, nil
}

func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.FromContext(ctx).Error(err, "writing response")
	}
}
