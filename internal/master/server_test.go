package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func baseTestConfig() Config {
	return Config{
		ListenAddr:          ":8001",
		AgentPort:           8002,
		AgentTaskPath:       "/api/v1/tasks",
		AgentHealthInterval: time.Minute,
		AgentHealthTimeout:  200 * time.Millisecond,
		SharedToken:         "shared-test-token",
		MaxParallel:         4,
		RegistryTimeout:     2 * time.Second,
		TaskDefaultTimeout:  2 * time.Second,
		TaskMaxTimeout:      5 * time.Second,
		AllowedOrigin:       "*",
	}
}

func TestNodesFiltersMasterAndDisabledEntries(t *testing.T) {
	t.Parallel()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []Node{
			{ID: "agent", IP: "127.0.0.1", Port: 8002, Status: "enabled"},
			{ID: "master", IP: "127.0.0.1", Port: 8001, Status: "enabled"},
			{ID: "disabled", IP: "127.0.0.2", Port: 8002, Status: "disabled"},
		}})
	}))
	defer registry.Close()
	cfg := baseTestConfig()
	cfg.RegistryURL = registry.URL
	server := httptest.NewServer(NewHandler(cfg, log.New(io.Discard, "", 0)))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("GET nodes: %v", err)
	}
	defer response.Body.Close()
	var payload struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	if len(payload.Nodes) != 1 || payload.Nodes[0].ID != "agent" {
		t.Fatalf("unexpected eligible nodes: %+v", payload.Nodes)
	}
	if payload.Nodes[0].HealthStatus != "offline" || payload.Nodes[0].HealthCheckedAt == nil {
		t.Fatalf("unexpected node health: %+v", payload.Nodes[0])
	}
}

func TestProbeDispatchesAndAggregates(t *testing.T) {
	t.Parallel()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "situation-awareness-agent"})
			return
		}
		if request.Header.Get("Authorization") != "Bearer shared-test-token" {
			t.Errorf("unexpected Authorization header: %q", request.Header.Get("Authorization"))
		}
		var task AgentTaskRequest
		if err := json.NewDecoder(request.Body).Decode(&task); err != nil {
			t.Errorf("decode task: %v", err)
		}
		_ = json.NewEncoder(w).Encode(AgentTaskResponse{
			Version: "v1", TaskID: task.TaskID, Type: task.Type, Target: task.Target, Status: "completed",
			Result: ProbeResult{
				NormalizedTarget: "https://example.com",
				Available:        true,
				DNS:              DNSResult{Addresses: []string{"93.184.216.34"}},
				HTTP:             []HTTPResult{{StatusCode: http.StatusOK}},
			},
		})
	}))
	defer agent.Close()
	agentURL, _ := url.Parse(agent.URL)
	agentHost, agentPortText, _ := net.SplitHostPort(agentURL.Host)
	agentPort, _ := strconv.Atoi(agentPortText)

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []Node{{ID: "beijing", IP: agentHost, Port: agentPort, Status: "enabled", City: "北京", ISP: "移动"}}})
	}))
	defer registry.Close()

	cfg := baseTestConfig()
	cfg.AgentPort = agentPort
	cfg.RegistryURL = registry.URL
	masterServer := httptest.NewServer(NewHandler(cfg, log.New(io.Discard, "", 0)))
	defer masterServer.Close()

	body := bytes.NewBufferString(`{"target":"example.com","nodeIds":["beijing"],"options":{"timeoutMs":1500,"ports":[80,443]}}`)
	response, err := http.Post(masterServer.URL+"/api/v1/probes", "application/json", body)
	if err != nil {
		t.Fatalf("POST probe: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, content)
	}
	var result ProbeResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode probe response: %v", err)
	}
	if result.Summary != (ProbeSummary{Total: 1, Completed: 1, Available: 1}) {
		t.Fatalf("unexpected summary: %+v", result.Summary)
	}
	if len(result.Results) != 1 || result.Results[0].Response == nil || result.Results[0].Response.TaskID != result.TaskID {
		t.Fatalf("unexpected results: %+v", result.Results)
	}
}

func TestProbeReturnsUnprocessableWhenSelectionDoesNotMatch(t *testing.T) {
	t.Parallel()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"nodes":[{"id":"beijing","ip":"127.0.0.1","port":8002,"status":"enabled"}]}`)
	}))
	defer registry.Close()
	cfg := baseTestConfig()
	cfg.RegistryURL = registry.URL
	server := httptest.NewServer(NewHandler(cfg, log.New(io.Discard, "", 0)))
	defer server.Close()

	response, err := http.Post(server.URL+"/api/v1/probes", "application/json", bytes.NewBufferString(`{"target":"example.com","nodeIds":["missing"]}`))
	if err != nil {
		t.Fatalf("POST probe: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func TestBatchProbeReturnsSummariesAndLazyTargetDetails(t *testing.T) {
	t.Parallel()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "situation-awareness-agent"})
			return
		}
		var task AgentTaskRequest
		if err := json.NewDecoder(request.Body).Decode(&task); err != nil {
			t.Errorf("decode task: %v", err)
		}
		available := task.Target != "down.example.com"
		httpResults := []HTTPResult{{Error: "context deadline exceeded"}}
		if available {
			httpResults = []HTTPResult{{StatusCode: http.StatusOK}}
		}
		_ = json.NewEncoder(w).Encode(AgentTaskResponse{
			Version: "v1", TaskID: task.TaskID, Type: task.Type, Target: task.Target, Status: "completed",
			Result: ProbeResult{
				NormalizedTarget: task.Target,
				Available:        available,
				DNS:              DNSResult{Addresses: []string{"127.0.0.1"}},
				HTTP:             httpResults,
			},
		})
	}))
	defer agent.Close()
	agentURL, _ := url.Parse(agent.URL)
	agentHost, agentPortText, _ := net.SplitHostPort(agentURL.Host)
	agentPort, _ := strconv.Atoi(agentPortText)

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []Node{{ID: "agent-1", IP: agentHost, Port: agentPort, Status: "enabled"}}})
	}))
	defer registry.Close()

	cfg := baseTestConfig()
	cfg.AgentPort = agentPort
	cfg.RegistryURL = registry.URL
	server := httptest.NewServer(NewHandler(cfg, log.New(io.Discard, "", 0)))
	defer server.Close()

	body := bytes.NewBufferString(`{"targets":["up.example.com","down.example.com"],"options":{"timeoutMs":1500,"ports":[80,443]}}`)
	response, err := http.Post(server.URL+"/api/v1/probes/batch", "application/json", body)
	if err != nil {
		t.Fatalf("POST batch: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, content)
	}
	var batch BatchProbeResponse
	if err := json.NewDecoder(response.Body).Decode(&batch); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	if batch.Status != "running" || batch.BatchTaskID == "" {
		t.Fatalf("unexpected initial batch response: %+v", batch)
	}

	deadline := time.Now().Add(3 * time.Second)
	for batch.Status != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("batch did not complete: %+v", batch)
		}
		time.Sleep(10 * time.Millisecond)
		summaryResponse, getErr := http.Get(fmt.Sprintf("%s/api/v1/probes/batch/%s", server.URL, batch.BatchTaskID))
		if getErr != nil {
			t.Fatalf("GET batch summary: %v", getErr)
		}
		if summaryResponse.StatusCode != http.StatusOK {
			content, _ := io.ReadAll(summaryResponse.Body)
			summaryResponse.Body.Close()
			t.Fatalf("summary status=%d body=%s", summaryResponse.StatusCode, content)
		}
		if err := json.NewDecoder(summaryResponse.Body).Decode(&batch); err != nil {
			summaryResponse.Body.Close()
			t.Fatalf("decode batch summary: %v", err)
		}
		summaryResponse.Body.Close()
	}
	if batch.Summary.TotalTargets != 2 || batch.Summary.AvailableTargets != 1 || batch.Summary.TotalNodeChecks != 2 {
		t.Fatalf("unexpected batch summary: %+v", batch.Summary)
	}
	if batch.Summary.AbnormalNodeChecks != 1 || batch.Summary.FailedNodeChecks != 0 {
		t.Fatalf("unexpected batch abnormal counts: %+v", batch.Summary)
	}
	if batch.Progress != 100 {
		t.Fatalf("unexpected completed progress: %d", batch.Progress)
	}
	if len(batch.Targets) != 2 || batch.Targets[0].Target != "up.example.com" || batch.Targets[1].Target != "down.example.com" {
		t.Fatalf("unexpected target summaries: %+v", batch.Targets)
	}
	if batch.Targets[0].Summary.Abnormal != 0 || batch.Targets[1].Summary.Abnormal != 1 {
		t.Fatalf("unexpected target abnormal counts: %+v", batch.Targets)
	}

	detailResponse, err := http.Get(fmt.Sprintf("%s/api/v1/probes/batch/%s/targets/0", server.URL, batch.BatchTaskID))
	if err != nil {
		t.Fatalf("GET batch target: %v", err)
	}
	defer detailResponse.Body.Close()
	if detailResponse.StatusCode != http.StatusOK {
		content, _ := io.ReadAll(detailResponse.Body)
		t.Fatalf("detail status=%d body=%s", detailResponse.StatusCode, content)
	}
	var detail ProbeResponse
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatalf("decode batch detail: %v", err)
	}
	if detail.Target != "up.example.com" || len(detail.Results) != 1 || detail.Results[0].Response == nil {
		t.Fatalf("unexpected target detail: %+v", detail)
	}

	missingResponse, err := http.Get(fmt.Sprintf("%s/api/v1/probes/batch/%s/targets/99", server.URL, batch.BatchTaskID))
	if err != nil {
		t.Fatalf("GET missing batch target: %v", err)
	}
	defer missingResponse.Body.Close()
	if missingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("missing detail status=%d", missingResponse.StatusCode)
	}
}

func TestProbeHasAbnormality(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result ProbeResult
		want   bool
	}{
		{
			name: "healthy HTTP response",
			result: ProbeResult{
				Available: true,
				DNS:       DNSResult{Addresses: []string{"192.0.2.1"}},
				HTTP:      []HTTPResult{{StatusCode: http.StatusOK}},
			},
		},
		{
			name: "HTTP timeout although TCP is reachable",
			result: ProbeResult{
				Available: true,
				DNS:       DNSResult{Addresses: []string{"192.0.2.1"}},
				TCP:       []TCPResult{{Port: 443, Reachable: true}},
				HTTP:      []HTTPResult{{Error: "context deadline exceeded"}},
			},
			want: true,
		},
		{
			name: "missing HTTP result",
			result: ProbeResult{
				Available: true,
				DNS:       DNSResult{Addresses: []string{"192.0.2.1"}},
				TCP:       []TCPResult{{Port: 443, Reachable: true}},
			},
			want: true,
		},
		{
			name: "DNS timeout alongside HTTP 200",
			result: ProbeResult{
				Available: true,
				DNS:       DNSResult{Error: "lookup example.com: i/o timeout"},
				HTTP:      []HTTPResult{{StatusCode: http.StatusOK}},
			},
			want: true,
		},
		{
			name: "HTTPS fallback fails but HTTP succeeds",
			result: ProbeResult{
				Available: true,
				DNS:       DNSResult{Addresses: []string{"192.0.2.1"}},
				HTTP: []HTTPResult{
					{Error: "TLS handshake failed"},
					{StatusCode: http.StatusOK},
				},
			},
		},
		{
			name: "HTTP 500 despite reachable TCP",
			result: ProbeResult{
				Available: true,
				DNS:       DNSResult{Addresses: []string{"192.0.2.1"}},
				TCP:       []TCPResult{{Port: 443, Reachable: true}},
				HTTP:      []HTTPResult{{StatusCode: http.StatusInternalServerError}},
			},
			want: true,
		},
		{
			name:   "unavailable result",
			result: ProbeResult{Available: false},
			want:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := probeHasAbnormality(test.result); got != test.want {
				t.Fatalf("probeHasAbnormality()=%t, want %t", got, test.want)
			}
		})
	}
}

func TestBatchProbeRejectsMoreThanTwoHundredTargets(t *testing.T) {
	t.Parallel()
	targets := make([]string, maxBatchTargets+1)
	for index := range targets {
		targets[index] = fmt.Sprintf("target-%d.example.com", index)
	}
	payload, err := json.Marshal(BatchProbeRequest{Targets: targets})
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	cfg := baseTestConfig()
	server := httptest.NewServer(NewHandler(cfg, log.New(io.Discard, "", 0)))
	defer server.Close()

	response, err := http.Post(server.URL+"/api/v1/probes/batch", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST oversized batch: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func TestNodesReportsOnlineAndOfflineAgentHealth(t *testing.T) {
	t.Parallel()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/healthz" {
			http.NotFound(w, request)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "situation-awareness-agent"})
	}))
	defer agent.Close()
	agentURL, _ := url.Parse(agent.URL)
	agentHost, agentPortText, _ := net.SplitHostPort(agentURL.Host)
	agentPort, _ := strconv.Atoi(agentPortText)

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []Node{
			{ID: "online-agent", IP: agentHost, Port: agentPort, Status: "enabled"},
			{ID: "offline-agent", IP: "127.0.0.2", Port: agentPort, Status: "enabled"},
		}})
	}))
	defer registry.Close()

	cfg := baseTestConfig()
	cfg.AgentPort = agentPort
	cfg.RegistryURL = registry.URL
	server := httptest.NewServer(NewHandler(cfg, log.New(io.Discard, "", 0)))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("GET nodes: %v", err)
	}
	defer response.Body.Close()
	var payload struct {
		Nodes      []Node `json:"nodes"`
		Count      int    `json:"count"`
		TotalCount int    `json:"totalCount"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	if payload.Count != 1 || payload.TotalCount != 2 || len(payload.Nodes) != 2 {
		t.Fatalf("unexpected node counts: %+v", payload)
	}
	statuses := map[string]string{}
	for _, node := range payload.Nodes {
		statuses[node.ID] = node.HealthStatus
		if node.HealthCheckedAt == nil {
			t.Fatalf("missing health check time: %+v", node)
		}
	}
	if statuses["online-agent"] != "online" || statuses["offline-agent"] != "offline" {
		t.Fatalf("unexpected health statuses: %+v", statuses)
	}

	probePayload, err := json.Marshal(ProbeRequest{
		Target:  "example.com",
		NodeIDs: []string{"offline-agent"},
	})
	if err != nil {
		t.Fatalf("encode probe request: %v", err)
	}
	probeResponse, err := http.Post(server.URL+"/api/v1/probes", "application/json", bytes.NewReader(probePayload))
	if err != nil {
		t.Fatalf("POST probe: %v", err)
	}
	defer probeResponse.Body.Close()
	if probeResponse.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("offline node probe status=%d", probeResponse.StatusCode)
	}
}

func TestNodeHealthMonitorRefreshesAgentState(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !healthy.Load() {
			http.Error(w, "stopped", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "situation-awareness-agent"})
	}))
	defer agent.Close()
	agentURL, _ := url.Parse(agent.URL)
	agentHost, agentPortText, _ := net.SplitHostPort(agentURL.Host)
	agentPort, _ := strconv.Atoi(agentPortText)

	healthNode := Node{ID: "agent", IP: agentHost, Port: agentPort, Status: "enabled"}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []Node{healthNode}})
	}))
	defer registry.Close()
	cfg := baseTestConfig()
	cfg.AgentPort = agentPort
	cfg.RegistryURL = registry.URL
	cfg.AgentHealthInterval = 20 * time.Millisecond
	cfg.AgentHealthTimeout = 100 * time.Millisecond
	service := newService(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.startNodeHealthMonitor(ctx, log.New(io.Discard, "", 0))

	healthKey := nodeHealthKey(healthNode)
	waitForHealthStatus(t, service, healthKey, "online")
	healthy.Store(false)
	waitForHealthStatus(t, service, healthKey, "offline")
}

func waitForHealthStatus(t *testing.T, service *service, nodeKey, expected string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		service.healthMu.RLock()
		status := service.nodeHealth[nodeKey].status
		service.healthMu.RUnlock()
		if status == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %s did not reach health status %s", nodeKey, expected)
}
