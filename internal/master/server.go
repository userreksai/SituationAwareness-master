package master

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type apiHandler struct {
	cfg     Config
	logger  *log.Logger
	service *service
}

func NewHandler(cfg Config, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	handler := &apiHandler{cfg: cfg, logger: logger, service: newService(cfg)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("GET /api/v1/health", handler.health)
	mux.HandleFunc("GET /api/v1/nodes", handler.nodes)
	mux.HandleFunc("POST /api/v1/probes", handler.probes)
	mux.HandleFunc("POST /api/v1/probes/batch", handler.probeBatch)
	mux.HandleFunc("GET /api/v1/probes/batch/{batchTaskID}", handler.probeBatchSummary)
	mux.HandleFunc("GET /api/v1/probes/batch/{batchTaskID}/targets/{targetIndex}", handler.probeBatchTarget)
	return handler.middleware(mux)
}

func (handler *apiHandler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"service":     "situation-awareness-master",
		"registryUrl": handler.cfg.RegistryURL,
		"agentPort":   handler.cfg.AgentPort,
		"time":        time.Now().UTC(),
	})
}

func (handler *apiHandler) nodes(w http.ResponseWriter, request *http.Request) {
	nodes, err := handler.service.listNodes(request.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "registry_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":     nodes,
		"count":     len(nodes),
		"fetchedAt": time.Now().UTC(),
	})
}

func (handler *apiHandler) probes(w http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(w, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var probe ProbeRequest
	if err := decoder.Decode(&probe); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", jsonErrorMessage(err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object")
		return
	}

	response, err := handler.service.run(request.Context(), probe)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoEligibleNodes):
			writeAPIError(w, http.StatusUnprocessableEntity, "no_eligible_nodes", "no enabled Agent on the configured port matched this request")
		case strings.HasPrefix(err.Error(), "registry:"):
			writeAPIError(w, http.StatusBadGateway, "registry_unavailable", err.Error())
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_probe", err.Error())
		}
		return
	}
	handler.logger.Printf("task=%s target=%q nodes=%d completed=%d failed=%d available=%d duration_ms=%d", response.TaskID, response.Target, response.Summary.Total, response.Summary.Completed, response.Summary.Failed, response.Summary.Available, response.DurationMS)
	writeJSON(w, http.StatusOK, response)
}

func (handler *apiHandler) probeBatch(w http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(w, request.Body, 512<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var batch BatchProbeRequest
	if err := decoder.Decode(&batch); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", jsonErrorMessage(err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object")
		return
	}

	response, err := handler.service.startBatch(request.Context(), batch)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoEligibleNodes):
			writeAPIError(w, http.StatusUnprocessableEntity, "no_eligible_nodes", "no enabled Agent on the configured port matched this request")
		case errors.Is(err, ErrBatchCapacity):
			writeAPIError(w, http.StatusTooManyRequests, "batch_capacity", err.Error())
		case strings.HasPrefix(err.Error(), "registry:"):
			writeAPIError(w, http.StatusBadGateway, "registry_unavailable", err.Error())
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_batch_probe", err.Error())
		}
		return
	}
	handler.logger.Printf(
		"batch=%s status=%s targets=%d nodes=%d accepted=true",
		response.BatchTaskID,
		response.Status,
		response.Summary.TotalTargets,
		response.NodeCount,
	)
	writeJSON(w, http.StatusAccepted, response)
}

func (handler *apiHandler) probeBatchSummary(w http.ResponseWriter, request *http.Request) {
	response, err := handler.service.batchSummary(strings.TrimSpace(request.PathValue("batchTaskID")))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "batch_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (handler *apiHandler) probeBatchTarget(w http.ResponseWriter, request *http.Request) {
	index, err := strconv.Atoi(request.PathValue("targetIndex"))
	if err != nil || index < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_target_index", "target index must be a non-negative integer")
		return
	}
	response, err := handler.service.batchTarget(strings.TrimSpace(request.PathValue("batchTaskID")), index)
	if err != nil {
		if errors.Is(err, ErrBatchNotFound) {
			writeAPIError(w, http.StatusNotFound, "batch_not_found", err.Error())
		} else {
			writeAPIError(w, http.StatusNotFound, "batch_target_not_found", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (handler *apiHandler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		origin := request.Header.Get("Origin")
		if handler.cfg.AllowedOrigin == "*" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" && origin == handler.cfg.AllowedOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if request.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, request)
	})
}

func jsonErrorMessage(err error) string {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return "request body is too large"
	}
	return fmt.Sprintf("invalid JSON: %v", err)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
