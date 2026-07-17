package master

import "time"

type Node struct {
	ID         string     `json:"id"`
	IP         string     `json:"ip"`
	Province   string     `json:"province"`
	City       string     `json:"city"`
	ISP        string     `json:"isp"`
	AgentName  string     `json:"agentName"`
	Port       int        `json:"port"`
	Status     string     `json:"status"`
	Remark     string     `json:"remark"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
}

type ProbeOptions struct {
	TimeoutMS int   `json:"timeoutMs,omitempty"`
	Ports     []int `json:"ports,omitempty"`
}

type ProbeRequest struct {
	Target  string       `json:"target"`
	NodeIDs []string     `json:"nodeIds,omitempty"`
	Options ProbeOptions `json:"options"`
}

type AgentTaskRequest struct {
	TaskID  string       `json:"taskId"`
	Type    string       `json:"type"`
	Target  string       `json:"target"`
	Options ProbeOptions `json:"options"`
}

type AgentTaskResponse struct {
	Version    string      `json:"version"`
	TaskID     string      `json:"taskId"`
	Type       string      `json:"type"`
	Target     string      `json:"target"`
	Agent      AgentInfo   `json:"agent"`
	Status     string      `json:"status"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt time.Time   `json:"finishedAt"`
	DurationMS int64       `json:"durationMs"`
	Result     ProbeResult `json:"result"`
}

type AgentInfo struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
}

type ProbeResult struct {
	NormalizedTarget string       `json:"normalizedTarget"`
	Available        bool         `json:"available"`
	DNS              DNSResult    `json:"dns"`
	TCP              []TCPResult  `json:"tcp"`
	HTTP             []HTTPResult `json:"http"`
}

type DNSResult struct {
	Addresses  []string `json:"addresses"`
	DurationMS int64    `json:"durationMs"`
	Error      string   `json:"error,omitempty"`
}

type TCPResult struct {
	Port       int    `json:"port"`
	Reachable  bool   `json:"reachable"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

type HTTPResult struct {
	URL         string `json:"url"`
	StatusCode  int    `json:"statusCode,omitempty"`
	DurationMS  int64  `json:"durationMs"`
	ContentType string `json:"contentType,omitempty"`
	Server      string `json:"server,omitempty"`
	Error       string `json:"error,omitempty"`
}

type NodeProbeResult struct {
	Node       Node               `json:"node"`
	Status     string             `json:"status"`
	DurationMS int64              `json:"durationMs"`
	Response   *AgentTaskResponse `json:"response,omitempty"`
	Error      string             `json:"error,omitempty"`
}

type ProbeSummary struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Available int `json:"available"`
}

type ProbeResponse struct {
	TaskID     string            `json:"taskId"`
	Target     string            `json:"target"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	DurationMS int64             `json:"durationMs"`
	Summary    ProbeSummary      `json:"summary"`
	Results    []NodeProbeResult `json:"results"`
}
