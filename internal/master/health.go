package master

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const agentHealthPath = "/healthz"

type agentHealthSnapshot struct {
	status    string
	checkedAt time.Time
	latencyMS int64
	error     string
}

type agentHealthResult struct {
	key      string
	snapshot agentHealthSnapshot
}

type agentHealthPayload struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
}

func (service *service) startNodeHealthMonitor(ctx context.Context, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	go func() {
		service.refreshRegisteredNodeHealth(ctx, logger)
		ticker := time.NewTicker(service.cfg.AgentHealthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				service.refreshRegisteredNodeHealth(ctx, logger)
			}
		}
	}()
}

func (service *service) refreshRegisteredNodeHealth(ctx context.Context, logger *log.Logger) {
	nodes, err := service.registry.list(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Printf("agent health registry refresh failed: %v", err)
		}
		return
	}
	service.refreshNodeHealth(ctx, eligibleNodes(nodes, service.cfg.AgentPort), true)
}

func (service *service) refreshNodeHealth(ctx context.Context, nodes []Node, force bool) {
	if len(nodes) == 0 {
		return
	}
	service.healthRefreshMu.Lock()
	defer service.healthRefreshMu.Unlock()
	if !force && service.nodeHealthFresh(nodes, time.Now().UTC()) {
		return
	}

	results := make(chan agentHealthResult, len(nodes))
	var waitGroup sync.WaitGroup
	for _, node := range nodes {
		waitGroup.Add(1)
		go func(node Node) {
			defer waitGroup.Done()
			select {
			case service.healthSemaphore <- struct{}{}:
				defer func() { <-service.healthSemaphore }()
			case <-ctx.Done():
				results <- agentHealthResult{key: nodeHealthKey(node), snapshot: offlineHealth(ctx.Err(), 0)}
				return
			}
			results <- agentHealthResult{key: nodeHealthKey(node), snapshot: service.checkNodeHealth(ctx, node)}
		}(node)
	}
	waitGroup.Wait()
	close(results)

	service.healthMu.Lock()
	for result := range results {
		service.nodeHealth[result.key] = result.snapshot
	}
	service.healthMu.Unlock()
}

func (service *service) nodeHealthFresh(nodes []Node, now time.Time) bool {
	oldestAllowed := now.Add(-service.cfg.AgentHealthInterval)
	service.healthMu.RLock()
	defer service.healthMu.RUnlock()
	for _, node := range nodes {
		snapshot, exists := service.nodeHealth[nodeHealthKey(node)]
		if !exists || snapshot.checkedAt.Before(oldestAllowed) {
			return false
		}
	}
	return true
}

func (service *service) checkNodeHealth(parent context.Context, node Node) agentHealthSnapshot {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, service.cfg.AgentHealthTimeout)
	defer cancel()
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(node.IP, strconv.Itoa(node.Port)), Path: agentHealthPath}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return offlineHealth(err, time.Since(started).Milliseconds())
	}
	request.Header.Set("Accept", "application/json")
	response, err := service.agentHTTP.Do(request)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return offlineHealth(err, latency)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return offlineHealth(fmt.Errorf("health endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body))), latency)
	}
	var payload agentHealthPayload
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&payload); err != nil {
		return offlineHealth(fmt.Errorf("decode health response: %w", err), latency)
	}
	if !payload.OK || !strings.EqualFold(payload.Service, "situation-awareness-agent") {
		return offlineHealth(fmt.Errorf("unexpected health response"), latency)
	}
	return agentHealthSnapshot{status: "online", checkedAt: time.Now().UTC(), latencyMS: latency}
}

func offlineHealth(err error, latencyMS int64) agentHealthSnapshot {
	message := "health check failed"
	if err != nil {
		message = err.Error()
	}
	if len(message) > 240 {
		message = message[:240]
	}
	return agentHealthSnapshot{status: "offline", checkedAt: time.Now().UTC(), latencyMS: latencyMS, error: message}
}

func (service *service) applyNodeHealth(nodes []Node) []Node {
	service.healthMu.RLock()
	defer service.healthMu.RUnlock()
	for index := range nodes {
		snapshot, exists := service.nodeHealth[nodeHealthKey(nodes[index])]
		if !exists {
			nodes[index].HealthStatus = "checking"
			continue
		}
		checkedAt := snapshot.checkedAt
		nodes[index].HealthStatus = snapshot.status
		nodes[index].HealthCheckedAt = &checkedAt
		nodes[index].HealthLatencyMS = snapshot.latencyMS
		nodes[index].HealthError = snapshot.error
	}
	return nodes
}

func onlineNodes(nodes []Node) []Node {
	result := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if node.HealthStatus == "online" {
			result = append(result, node)
		}
	}
	return result
}

func nodeHealthKey(node Node) string {
	endpoint := net.JoinHostPort(strings.TrimSpace(node.IP), strconv.Itoa(node.Port))
	if id := strings.TrimSpace(node.ID); id != "" {
		return id + "|" + endpoint
	}
	return endpoint
}
