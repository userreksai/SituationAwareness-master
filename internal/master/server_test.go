package master

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func baseTestConfig() Config {
	return Config{
		ListenAddr:         ":8001",
		AgentPort:          8002,
		AgentTaskPath:      "/api/v1/tasks",
		SharedToken:        "shared-test-token",
		MaxParallel:        4,
		RegistryTimeout:    2 * time.Second,
		TaskDefaultTimeout: 2 * time.Second,
		TaskMaxTimeout:     5 * time.Second,
		AllowedOrigin:      "*",
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
}

func TestProbeDispatchesAndAggregates(t *testing.T) {
	t.Parallel()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer shared-test-token" {
			t.Errorf("unexpected Authorization header: %q", request.Header.Get("Authorization"))
		}
		var task AgentTaskRequest
		if err := json.NewDecoder(request.Body).Decode(&task); err != nil {
			t.Errorf("decode task: %v", err)
		}
		_ = json.NewEncoder(w).Encode(AgentTaskResponse{
			Version: "v1", TaskID: task.TaskID, Type: task.Type, Target: task.Target, Status: "completed",
			Result: ProbeResult{NormalizedTarget: "https://example.com", Available: true, DNS: DNSResult{Addresses: []string{"93.184.216.34"}}},
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
