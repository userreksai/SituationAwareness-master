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

var ErrNoEligibleNodes = errors.New("no eligible agent nodes")

type service struct {
	cfg       Config
	registry  *registryClient
	agentHTTP *http.Client
	semaphore chan struct{}
}

func newService(cfg Config) *service {
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
		cfg:       cfg,
		registry:  newRegistryClient(cfg),
		agentHTTP: &http.Client{Transport: transport},
		semaphore: make(chan struct{}, cfg.MaxParallel),
	}
}

func (service *service) listNodes(ctx context.Context) ([]Node, error) {
	nodes, err := service.registry.list(ctx)
	if err != nil {
		return nil, err
	}
	return eligibleNodes(nodes, service.cfg.AgentPort), nil
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
	if len(nodes) == 0 {
		return ProbeResponse{}, ErrNoEligibleNodes
	}

	started := time.Now().UTC()
	taskID := newTaskID()
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
		} else {
			response.Summary.Failed++
		}
	}
	return response, nil
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
