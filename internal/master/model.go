package master

import "time"

type Node struct {
	ID              string     `json:"id"`
	IP              string     `json:"ip"`
	Province        string     `json:"province"`
	City            string     `json:"city"`
	ISP             string     `json:"isp"`
	AgentName       string     `json:"agentName"`
	Port            int        `json:"port"`
	Status          string     `json:"status"`
	HealthStatus    string     `json:"healthStatus"`
	HealthCheckedAt *time.Time `json:"healthCheckedAt,omitempty"`
	HealthLatencyMS int64      `json:"healthLatencyMs,omitempty"`
	HealthError     string     `json:"healthError,omitempty"`
	Remark          string     `json:"remark"`
	CreatedAt       *time.Time `json:"createdAt,omitempty"`
	UpdatedAt       *time.Time `json:"updatedAt,omitempty"`
	LastSeenAt      *time.Time `json:"lastSeenAt,omitempty"`
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

type BatchProbeRequest struct {
	Targets []string     `json:"targets"`
	NodeIDs []string     `json:"nodeIds,omitempty"`
	Options ProbeOptions `json:"options"`
}

type BatchTargetSummary struct {
	Index      int          `json:"index"`
	TaskID     string       `json:"taskId"`
	Target     string       `json:"target"`
	Status     string       `json:"status"`
	DurationMS int64        `json:"durationMs"`
	Summary    ProbeSummary `json:"summary"`
}

type BatchProbeSummary struct {
	TotalTargets        int `json:"totalTargets"`
	CompletedTargets    int `json:"completedTargets"`
	FailedTargets       int `json:"failedTargets"`
	AvailableTargets    int `json:"availableTargets"`
	TotalNodeChecks     int `json:"totalNodeChecks"`
	AvailableNodeChecks int `json:"availableNodeChecks"`
}

// BatchProbeResponse intentionally contains target-level summaries only.
// Node-level details are retained by Master and fetched on demand per target.
type BatchProbeResponse struct {
	BatchTaskID string               `json:"batchTaskId"`
	Status      string               `json:"status"`
	Progress    int                  `json:"progress"`
	Error       string               `json:"error,omitempty"`
	StartedAt   time.Time            `json:"startedAt"`
	FinishedAt  time.Time            `json:"finishedAt"`
	ExpiresAt   time.Time            `json:"expiresAt"`
	DurationMS  int64                `json:"durationMs"`
	NodeCount   int                  `json:"nodeCount"`
	Summary     BatchProbeSummary    `json:"summary"`
	Targets     []BatchTargetSummary `json:"targets"`
}
