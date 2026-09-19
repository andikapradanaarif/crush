package tools

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed todos.md
var todosDescription string

const TodosToolName = "todos"

type TodosParams struct {
	Todos []TodoItem `json:"todos" description:"The updated todo list"`
}

type TodoItem struct {
	Content    string `json:"content" description:"What needs to be done (imperative form)"`
	Status     string `json:"status" description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g., 'Running tests')"`
	// Key is the model-authored slug other items reference in
	// depends_on. Keep it stable across rewrites — it is the item's
	// identity; rewording content does not change it.
	Key string `json:"key,omitempty" description:"Stable slug identifying this item; required on items other items depend on"`
	// DependsOn lists keys of items that must complete before this
	// one. The write maps keys to harness-minted IDs.
	DependsOn []string `json:"depends_on,omitempty" description:"Keys of items that must complete before this one"`
	// EvidenceChecks binds named checks: the item counts as done only
	// when the latest instance of each name resolved green. Only
	// configured check names are bindable.
	EvidenceChecks []string `json:"evidence_checks,omitempty" description:"Named checks that must resolve green for this item to count as done"`
	// EvidencePaths declares the files or directories the item's work
	// touches: done requires an observed write on the path and no
	// covering check failed. Also serves as the item's map in repair
	// prompts.
	EvidencePaths []string `json:"evidence_paths,omitempty" description:"Files or directories this item's work must touch"`
}

type TodosResponseMetadata struct {
	IsNew         bool               `json:"is_new"`
	Todos         []session.PlanItem `json:"todos"`
	JustCompleted []string           `json:"just_completed,omitempty"`
	JustStarted   string             `json:"just_started,omitempty"`
	Completed     int                `json:"completed"`
	Total         int                `json:"total"`
}

// NewTodosTool builds the plan tool. checkNames is the bindable
// evidence-check vocabulary — the configured verify commands' check
// identities — surfaced in the description so the model can bind names
// it has never seen run.
func NewTodosTool(sessions session.Service, checkNames []string, workingDir string) fantasy.AgentTool {
	description := todosDescription
	bindable := map[string]bool{}
	var names []string
	for _, name := range checkNames {
		if !bindable[name] {
			bindable[name] = true
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		var b strings.Builder
		b.WriteString("\n\nBindable `evidence_checks` names (anything else is rejected):\n")
		for _, name := range names {
			b.WriteString("- " + name + "\n")
		}
		description += b.String()
	}

	return fantasy.NewAgentTool(
		TodosToolName,
		description,
		func(ctx context.Context, params TodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for managing todos")
			}

			currentSession, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			if err := validatePlanItems(params.Todos, bindable, workingDir); err != nil {
				return fantasy.ToolResponse{}, err
			}

			isNew := len(currentSession.Todos) == 0
			oldStatusByID := make(map[string]session.PlanItemStatus)
			for _, todo := range currentSession.Todos {
				oldStatusByID[todo.ID] = todo.Status
			}

			todos := make([]session.PlanItem, len(params.Todos))
			var justCompleted []string
			var justStarted string
			completedCount := 0

			for i, item := range params.Todos {
				id := session.MintPlanItemID(item.Key, item.Content)
				dependsOn := make([]string, 0, len(item.DependsOn))
				for _, key := range item.DependsOn {
					dependsOn = append(dependsOn, session.MintPlanItemID(key, ""))
				}
				todos[i] = session.PlanItem{
					ID:             id,
					Key:            item.Key,
					Content:        item.Content,
					Status:         session.PlanItemStatus(item.Status),
					ActiveForm:     item.ActiveForm,
					DependsOn:      dependsOn,
					EvidenceChecks: item.EvidenceChecks,
					EvidencePaths:  item.EvidencePaths,
				}

				newStatus := session.PlanItemStatus(item.Status)
				oldStatus, existed := oldStatusByID[id]

				if newStatus == session.PlanItemCompleted {
					completedCount++
					if existed && oldStatus != session.PlanItemCompleted {
						justCompleted = append(justCompleted, item.Content)
					}
				}

				if newStatus == session.PlanItemInProgress {
					if !existed || oldStatus != session.PlanItemInProgress {
						if item.ActiveForm != "" {
							justStarted = item.ActiveForm
						} else {
							justStarted = item.Content
						}
					}
				}
			}

			currentSession.Todos = todos
			_, err = sessions.Save(ctx, currentSession)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save todos: %w", err)
			}

			response := "Todo list updated successfully.\n\n"

			pendingCount := 0
			inProgressCount := 0

			for _, todo := range todos {
				switch todo.Status {
				case session.PlanItemPending:
					pendingCount++
				case session.PlanItemInProgress:
					inProgressCount++
				}
			}

			response += fmt.Sprintf("Status: %d pending, %d in progress, %d completed\n",
				pendingCount, inProgressCount, completedCount)

			response += "Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable."

			metadata := TodosResponseMetadata{
				IsNew:         isNew,
				Todos:         todos,
				JustCompleted: justCompleted,
				JustStarted:   justStarted,
				Completed:     completedCount,
				Total:         len(todos),
			}

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
		},
	)
}

// validatePlanItems enforces the plan's structural rules on write:
// valid statuses, unique keys, distinct contents (identical contents
// are ambiguous dep targets), depends_on resolving to keys present in
// the same list, no cycles or self-deps, and only configured check
// names bound. Every rejection names the offending key or item so the
// model can repair in the same call.
func validatePlanItems(items []TodoItem, bindable map[string]bool, workingDir string) error {
	keys := map[string]bool{}
	contents := map[string]int{}
	cleanWD := filepath.Clean(workingDir)
	for i, item := range items {
		switch item.Status {
		case "pending", "in_progress", "completed":
		default:
			return fmt.Errorf("invalid status %q for todo %q", item.Status, item.Content)
		}
		if item.Key != "" {
			if keys[item.Key] {
				return fmt.Errorf("duplicate key %q on item %d (%q) — keys must be unique within the list", item.Key, i, item.Content)
			}
			keys[item.Key] = true
		}
		if first, dup := contents[item.Content]; dup {
			return fmt.Errorf("items %d and %d have identical content %q — make them distinct (ambiguous dependency target)", first, i, item.Content)
		}
		contents[item.Content] = i
	}
	for i, item := range items {
		for _, dep := range item.DependsOn {
			if dep == item.Key && item.Key != "" {
				return fmt.Errorf("item %q depends on itself", item.Key)
			}
			if !keys[dep] {
				return fmt.Errorf("item %d (%q) depends on unknown key %q — depends_on references keys of items in this list", i, item.Content, dep)
			}
		}
		for _, check := range item.EvidenceChecks {
			if strings.TrimSpace(check) == "" {
				return fmt.Errorf("item %q binds an empty evidence_checks entry — name a configured check", item.Content)
			}
			if !bindable[check] {
				return fmt.Errorf("item %q binds unknown check %q — only configured check names are bindable (see tool description)", item.Content, check)
			}
		}
		for _, path := range item.EvidencePaths {
			if strings.TrimSpace(path) == "" {
				return fmt.Errorf("item %q binds an empty evidence_paths entry — name a file or directory the work touches", item.Content)
			}
			// A binding that normalizes to the working directory (or an
			// ancestor of it) covers every write — vacuous evidence.
			np := filepath.Clean(filepathext.SmartJoin(cleanWD, strings.TrimRight(path, "/\\")))
			rel, err := filepath.Rel(np, cleanWD)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("item %q binds %q which covers the whole working directory — name a specific file or directory", item.Content, path)
			}
		}
	}
	if err := detectPlanCycle(items); err != nil {
		return err
	}
	return nil
}

// detectPlanCycle rejects dependency cycles over the submitted key
// graph, naming one member of the cycle so the model can repair it.
func detectPlanCycle(items []TodoItem) error {
	deps := map[string][]string{}
	for _, item := range items {
		if item.Key != "" {
			deps[item.Key] = item.DependsOn
		}
	}
	const (
		white = iota // unvisited
		gray         // on the current DFS stack
		black        // fully explored
	)
	color := map[string]int{}
	var visit func(key string) error
	visit = func(key string) error {
		color[key] = gray
		for _, dep := range deps[key] {
			switch color[dep] {
			case gray:
				return fmt.Errorf("dependency cycle: item %q depends on %q which (transitively) depends back", key, dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[key] = black
		return nil
	}
	for _, item := range items {
		if item.Key != "" && color[item.Key] == white {
			if err := visit(item.Key); err != nil {
				return err
			}
		}
	}
	return nil
}
