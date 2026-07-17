package master

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

type registryClient struct {
	url    string
	client *http.Client
}

type registryResponse struct {
	Nodes []Node `json:"nodes"`
}

func newRegistryClient(cfg Config) *registryClient {
	return &registryClient{url: cfg.RegistryURL, client: &http.Client{Timeout: cfg.RegistryTimeout}}
}

func (client *registryClient) list(ctx context.Context) ([]Node, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create registry request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request node registry: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("node registry returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload registryResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode node registry response: %w", err)
	}
	if payload.Nodes == nil {
		payload.Nodes = []Node{}
	}
	return payload.Nodes, nil
}

func eligibleNodes(nodes []Node, agentPort int) []Node {
	result := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if !strings.EqualFold(strings.TrimSpace(node.Status), "enabled") || node.Port != agentPort || net.ParseIP(strings.TrimSpace(node.IP)) == nil {
			continue
		}
		node.IP = strings.TrimSpace(node.IP)
		result = append(result, node)
	}
	return result
}
