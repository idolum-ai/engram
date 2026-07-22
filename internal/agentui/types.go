// Package agentui interprets bounded terminal frames as conservative,
// tool-independent agent presentation. Unknown text always remains literal.
package agentui

type Role string

const (
	RoleUnknown        Role = "unknown"
	RoleUserMessage    Role = "user_message"
	RoleAssistant      Role = "assistant_message"
	RoleToolInvocation Role = "tool_invocation"
	RoleToolResult     Role = "tool_result"
	RoleApproval       Role = "approval"
	RoleActivity       Role = "activity"
	RoleComposer       Role = "composer"
	RoleStatus         Role = "status"
	RoleChrome         Role = "chrome"
)

type Activity string

const (
	ActivityUnknown          Activity = "unknown"
	ActivityIdle             Activity = "idle"
	ActivityActive           Activity = "active"
	ActivityAwaitingApproval Activity = "awaiting_approval"
)

type Frame struct {
	Text            string `json:"text"`
	CurrentCommand  string `json:"current_command,omitempty"`
	Columns         int    `json:"columns,omitempty"`
	VisibleRows     int    `json:"visible_rows,omitempty"`
	AlternateScreen string `json:"alternate_screen,omitempty"`
	CopyMode        string `json:"copy_mode,omitempty"`
}

type Observation struct {
	Current  Frame  `json:"current"`
	Previous *Frame `json:"previous,omitempty"`
}

type Region struct {
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	Role       Role     `json:"role"`
	Confidence int      `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"`
	Omitted    bool     `json:"omitted,omitempty"`
}

type Analysis struct {
	Original     string   `json:"original"`
	Conversation string   `json:"conversation"`
	Regions      []Region `json:"regions,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	Activity     Activity `json:"activity"`
	Confidence   int      `json:"confidence"`
	Applied      bool     `json:"applied"`
}
