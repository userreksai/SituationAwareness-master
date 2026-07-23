package master

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNoEligibleNodes = errors.New("no eligible agent nodes")
	ErrBatchNotFound   = errors.New("batch task not found or expired")
	ErrBatchTarget     = errors.New("batch target not found")
	ErrBatchCapacity   = errors.New("too many batch tasks are still running")
)

const (
	maxBatchTargets    = 200
	maxBatchNodeChecks = 20000
	maxStoredBatches   = 10
	batchTargetWorkers = 10
	batchResultTTL     = 30 * time.Minute
	batchRuntimeLimit  = 30 * time.Minute
)

type storedBatchResult struct {
	response BatchProbeResponse
	details  []ProbeResponse
}

type service struct {
	cfg             Config
	registry        *registryClient
	agentHTTP       *http.Client
	semaphore       chan struct{}
	healthSemaphore chan struct{}
	healthRefreshMu sync.Mutex
	healthMu        sync.RWMutex
	nodeHealth      map[string]agentHealthSnapshot
	batchMu         sync.RWMutex
	batches         map[string]storedBatchResult
}

func newService(cfg Config) *service {
	if cfg.AgentHealthInterval <= 0 {
		cfg.AgentHealthInterval = time.Minute
	}
	if cfg.AgentHealthTimeout <= 0 {
		cfg.AgentHealthTimeout = 3 * time.Second
	}
	healthParallel := cfg.MaxParallel
	if healthParallel > 20 {
		healthParallel = 20
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{}).DialContext,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        cfg.MaxParallel * 2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &service{
		cfg:             cfg,
		registry:        newRegistryClient(cfg),
		agentHTTP:       &http.Client{Transport: transport},
		semaphore:       make(chan struct{}, cfg.MaxParallel),
		healthSemaphore: make(chan struct{}, healthParallel),
		nodeHealth:      make(map[string]agentHealthSnapshot),
		batches:         make(map[string]storedBatchResult),
	}
}

func (service *service) listNodes(ctx context.Context) ([]Node, error) {
	nodes, err := service.registry.list(ctx)
	if err != nil {
		return nil, err
	}
	nodes = eligibleNodes(nodes, service.cfg.AgentPort)
	service.refreshNodeHealth(ctx, nodes, false)
	return service.applyNodeHealth(nodes), nil
}

func (service *service) run(ctx context.Context, request ProbeRequest) (ProbeResponse, error) {
	if err := service.validateRequest(&request); err != nil {
		return ProbeResponse{}, err
	}
	nodes, err := service.listNodes(ctx)
	if err != nil {
		return ProbeResponse{}, fmt.Errorf("registry: %w", err)
	}
	nodes = selectNodes(nodes, request.NodeIDs)
	nodes = onlineNodes(nodes)
	if len(nodes) == 0 {
		return ProbeResponse{}, ErrNoEligibleNodes
	}
	return service.runWithNodes(ctx, request, nodes, newTaskID()), nil
}

func (service *service) runWithNodes(ctx context.Context, request ProbeRequest, nodes []Node, taskID string) ProbeResponse {
	started := time.Now().UTC()
	results := make([]NodeProbeResult, len(nodes))
	var wg sync.WaitGroup
	for index, node := range nodes {
		wg.Add(1)
		go func(index int, node Node) {
			defer wg.Done()
			select {
			case service.semaphore <- struct{}{}:
				defer func() { <-service.semaphore }()
			case <-ctx.Done():
				results[index] = NodeProbeResult{Node: node, Status: "failed", Error: ctx.Err().Error()}
				return
			}
			results[index] = service.dispatch(ctx, node, AgentTaskRequest{TaskID: taskID, Type: "probe", Target: request.Target, Options: request.Options})
		}(index, node)
	}
	wg.Wait()

	finished := time.Now().UTC()
	response := ProbeResponse{
		TaskID:     taskID,
		Target:     request.Target,
		StartedAt:  started,
		FinishedAt: finished,
		DurationMS: finished.Sub(started).Milliseconds(),
		Results:    results,
		Summary:    ProbeSummary{Total: len(results)},
	}
	for _, result := range results {
		if result.Status == "completed" {
			response.Summary.Completed++
			if result.Response != nil && result.Response.Result.Available {
				response.Summary.Available++
			}
			if result.Response == nil || probeHasAbnormality(result.Response.Result) {
				response.Summary.Abnormal++
			}
		} else {
			response.Summary.Failed++
		}
	}
	return response
}

func probeHasAbnormality(result ProbeResult) bool {
	if strings.TrimSpace(result.DNS.Error) != "" || !result.Available {
		return true
	}
	if len(result.HTTP) == 0 {
		return true
	}
	for _, item := range result.HTTP {
		if item.StatusCode >= 100 && item.StatusCode < 500 {
			return false
		}
	}
	return true
}

func (service *service) startBatch(ctx context.Context, request BatchProbeRequest) (BatchProbeResponse, error) {
	if len(request.Targets) == 0 {
		return BatchProbeResponse{}, fmt.Errorf("targets is required")
	}
	if len(request.Targets) > maxBatchTargets {
		return BatchProbeResponse{}, fmt.Errorf("targets supports at most %d entries", maxBatchTargets)
	}

	normalized := make([]ProbeRequest, 0, len(request.Targets))
	seenTargets := make(map[string]struct{}, len(request.Targets))
	for index, target := range request.Targets {
		probe := ProbeRequest{Target: target, NodeIDs: request.NodeIDs, Options: request.Options}
		if err := service.validateRequest(&probe); err != nil {
			return BatchProbeResponse{}, fmt.Errorf("targets[%d]: %w", index, err)
		}
		if _, exists := seenTargets[probe.Target]; exists {
			continue
		}
		seenTargets[probe.Target] = struct{}{}
		normalized = append(normalized, probe)
	}

	nodes, err := service.listNodes(ctx)
	if err != nil {
		return BatchProbeResponse{}, fmt.Errorf("registry: %w", err)
	}
	nodes = selectNodes(nodes, request.NodeIDs)
	nodes = onlineNodes(nodes)
	if len(nodes) == 0 {
		return BatchProbeResponse{}, ErrNoEligibleNodes
	}
	if len(normalized)*len(nodes) > maxBatchNodeChecks {
		return BatchProbeResponse{}, fmt.Errorf("batch expands to %d node checks; maximum is %d", len(normalized)*len(nodes), maxBatchNodeChecks)
	}

	started := time.Now().UTC()
	response := BatchProbeResponse{
		BatchTaskID: "batch-" + strings.TrimPrefix(newTaskID(), "sa-"),
		Status:      "running",
		Progress:    0,
		StartedAt:   started,
		ExpiresAt:   started.Add(batchRuntimeLimit),
		NodeCount:   len(nodes),
		Summary:     BatchProbeSummary{TotalTargets: len(normalized)},
		Targets:     make([]BatchTargetSummary, len(normalized)),
	}
	for index, probe := range normalized {
		response.Targets[index] = BatchTargetSummary{Index: index, Target: probe.Target, Status: "pending"}
	}
	details := make([]ProbeResponse, len(normalized))
	if err := service.storeBatch(response, details); err != nil {
		return BatchProbeResponse{}, err
	}

	storedResponse := cloneBatchResponse(response)
	go service.executeBatch(response.BatchTaskID, normalized, nodes)
	return storedResponse, nil
}

func (service *service) executeBatch(batchTaskID string, probes []ProbeRequest, nodes []Node) {
	ctx, cancel := context.WithTimeout(context.Background(), batchRuntimeLimit)
	defer cancel()

	jobs := make(chan int)
	workerCount := batchTargetWorkers
	if workerCount > service.cfg.MaxParallel {
		workerCount = service.cfg.MaxParallel
	}
	if workerCount > len(probes) {
		workerCount = len(probes)
	}

	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				detail := service.runWithNodes(ctx, probes[index], nodes, newTaskID())
				service.storeBatchTarget(batchTaskID, index, detail)
			}
		}()
	}
	for index := range probes {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	service.finishBatch(batchTaskID)
}

func (service *service) storeBatchTarget(batchTaskID string, index int, detail ProbeResponse) {
	service.batchMu.Lock()
	defer service.batchMu.Unlock()
	batch, exists := service.batches[batchTaskID]
	if !exists || index < 0 || index >= len(batch.details) {
		return
	}
	batch.details[index] = detail
	status := "completed"
	if detail.Summary.Completed == 0 {
		status = "failed"
		batch.response.Summary.FailedTargets++
	} else {
		batch.response.Summary.CompletedTargets++
	}
	if detail.Summary.Available > 0 {
		batch.response.Summary.AvailableTargets++
	}
	batch.response.Summary.TotalNodeChecks += detail.Summary.Total
	batch.response.Summary.AvailableNodeChecks += detail.Summary.Available
	batch.response.Summary.AbnormalNodeChecks += detail.Summary.Abnormal
	batch.response.Summary.FailedNodeChecks += detail.Summary.Failed
	batch.response.Targets[index] = BatchTargetSummary{
		Index:      index,
		TaskID:     detail.TaskID,
		Target:     detail.Target,
		Status:     status,
		DurationMS: detail.DurationMS,
		Summary:    detail.Summary,
	}
	done := batch.response.Summary.CompletedTargets + batch.response.Summary.FailedTargets
	batch.response.Progress = done * 100 / batch.response.Summary.TotalTargets
	service.batches[batchTaskID] = batch
}

func (service *service) finishBatch(batchTaskID string) {
	service.batchMu.Lock()
	defer service.batchMu.Unlock()
	batch, exists := service.batches[batchTaskID]
	if !exists {
		return
	}
	finished := time.Now().UTC()
	batch.response.Status = "completed"
	batch.response.Progress = 100
	batch.response.FinishedAt = finished
	batch.response.ExpiresAt = finished.Add(batchResultTTL)
	batch.response.DurationMS = finished.Sub(batch.response.StartedAt).Milliseconds()
	service.batches[batchTaskID] = batch
}

func cloneBatchResponse(response BatchProbeResponse) BatchProbeResponse {
	response.Targets = append([]BatchTargetSummary(nil), response.Targets...)
	return response
}

func (service *service) storeBatch(response BatchProbeResponse, details []ProbeResponse) error {
	service.batchMu.Lock()
	defer service.batchMu.Unlock()
	now := time.Now().UTC()
	for taskID, batch := range service.batches {
		if !batch.response.ExpiresAt.After(now) {
			delete(service.batches, taskID)
		}
	}
	if len(service.batches) >= maxStoredBatches {
		var oldestID string
		var oldestFinished time.Time
		for taskID, batch := range service.batches {
			if batch.response.Status == "running" {
				continue
			}
			if oldestID == "" || batch.response.FinishedAt.Before(oldestFinished) {
				oldestID = taskID
				oldestFinished = batch.response.FinishedAt
			}
		}
		if oldestID == "" {
			return ErrBatchCapacity
		}
		delete(service.batches, oldestID)
	}
	response = cloneBatchResponse(response)
	service.batches[response.BatchTaskID] = storedBatchResult{response: response, details: details}
	return nil
}

func (service *service) batchSummary(taskID string) (BatchProbeResponse, error) {
	service.batchMu.RLock()
	batch, exists := service.batches[taskID]
	response := cloneBatchResponse(batch.response)
	service.batchMu.RUnlock()
	if !exists || !response.ExpiresAt.After(time.Now().UTC()) {
		if exists {
			service.batchMu.Lock()
			delete(service.batches, taskID)
			service.batchMu.Unlock()
		}
		return BatchProbeResponse{}, ErrBatchNotFound
	}
	return response, nil
}

func (service *service) batchTarget(taskID string, index int) (ProbeResponse, error) {
	service.batchMu.RLock()
	batch, exists := service.batches[taskID]
	if exists && index >= 0 && index < len(batch.details) {
		detail := batch.details[index]
		service.batchMu.RUnlock()
		if !batch.response.ExpiresAt.After(time.Now().UTC()) {
			service.batchMu.Lock()
			delete(service.batches, taskID)
			service.batchMu.Unlock()
			return ProbeResponse{}, ErrBatchNotFound
		}
		if detail.TaskID == "" {
			return ProbeResponse{}, ErrBatchTarget
		}
		return detail, nil
	}
	service.batchMu.RUnlock()
	if !exists || !batch.response.ExpiresAt.After(time.Now().UTC()) {
		if exists {
			service.batchMu.Lock()
			delete(service.batches, taskID)
			service.batchMu.Unlock()
		}
		return ProbeResponse{}, ErrBatchNotFound
	}
	if index < 0 || index >= len(batch.details) {
		return ProbeResponse{}, ErrBatchTarget
	}
	return ProbeResponse{}, ErrBatchTarget
}

func (service *service) validateRequest(request *ProbeRequest) error {
	request.Target = strings.TrimSpace(request.Target)
	if request.Target == "" {
		return fmt.Errorf("target is required")
	}
	if len(request.Target) > 2048 {
		return fmt.Errorf("target must be at most 2048 characters")
	}
	if strings.ContainsAny(request.Target, "\r\n\x00") {
		return fmt.Errorf("target contains invalid characters")
	}
	if request.Options.TimeoutMS == 0 {
		request.Options.TimeoutMS = int(service.cfg.TaskDefaultTimeout / time.Millisecond)
	}
	requestedTimeout := time.Duration(request.Options.TimeoutMS) * time.Millisecond
	if requestedTimeout < 500*time.Millisecond || requestedTimeout > service.cfg.TaskMaxTimeout {
		return fmt.Errorf("options.timeoutMs must be between 500 and %d", service.cfg.TaskMaxTimeout.Milliseconds())
	}
	if len(request.Options.Ports) > 10 {
		return fmt.Errorf("options.ports supports at most 10 ports")
	}
	seenPorts := make(map[int]struct{}, len(request.Options.Ports))
	ports := make([]int, 0, len(request.Options.Ports))
	for _, port := range request.Options.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("options.ports values must be between 1 and 65535")
		}
		if _, exists := seenPorts[port]; !exists {
			seenPorts[port] = struct{}{}
			ports = append(ports, port)
		}
	}
	request.Options.Ports = ports
	if len(request.NodeIDs) > 500 {
		return fmt.Errorf("nodeIds supports at most 500 nodes")
	}
	return nil
}

func selectNodes(nodes []Node, requestedIDs []string) []Node {
	if len(requestedIDs) == 0 {
		return nodes
	}
	requested := make(map[string]struct{}, len(requestedIDs))
	for _, id := range requestedIDs {
		if id = strings.TrimSpace(id); id != "" {
			requested[id] = struct{}{}
		}
	}
	selected := make([]Node, 0, len(requested))
	for _, node := range nodes {
		if _, ok := requested[node.ID]; ok {
			selected = append(selected, node)
		}
	}
	return selected
}

func (service *service) dispatch(parent context.Context, node Node, task AgentTaskRequest) NodeProbeResult {
	started := time.Now()
	result := NodeProbeResult{Node: node, Status: "failed"}
	payload, err := json.Marshal(task)
	if err != nil {
		result.Error = fmt.Sprintf("encode task: %v", err)
		return result
	}
	requestTimeout := time.Duration(task.Options.TimeoutMS)*time.Millisecond + 2*time.Second
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(node.IP, strconv.Itoa(node.Port)), Path: service.cfg.AgentTaskPath}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		result.Error = fmt.Sprintf("create Agent request: %v", err)
		return result
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if service.cfg.SharedToken != "" {
		request.Header.Set("Authorization", "Bearer "+service.cfg.SharedToken)
	}
	response, err := service.agentHTTP.Do(request)
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = fmt.Sprintf("call Agent: %v", err)
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		result.Error = fmt.Sprintf("Agent returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		return result
	}
	var agentResponse AgentTaskResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&agentResponse); err != nil {
		result.Error = fmt.Sprintf("decode Agent response: %v", err)
		return result
	}
	if agentResponse.TaskID != task.TaskID {
		result.Error = "Agent response taskId does not match the dispatched task"
		return result
	}
	result.Status = "completed"
	result.Response = &agentResponse
	return result
}

func newTaskID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("sa-%d", time.Now().UnixNano())
	}
	return "sa-" + hex.EncodeToString(value[:])
}
