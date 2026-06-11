package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ToolIndex struct {
	Tools []IndexEntry `json:"tools"`
}

type IndexEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ToolDef struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Executable   string   `json:"executable"`
	DangerLevel  string   `json:"danger_level"`
	Arguments    []ArgDef `json:"arguments"`
	Examples     []string `json:"examples"`
	resolvedExec string
}

type ArgDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Required    bool        `json:"required"`
	Positional  int         `json:"positional,omitempty"`
	Flag        string      `json:"flag,omitempty"`
	Default     interface{} `json:"default,omitempty"`
}

type ChainStep struct {
	Tool   string                 `json:"tool"`
	Params map[string]interface{} `json:"params"`
}

type LLMResponse struct {
	Tool   string                 `json:"tool,omitempty"`
	Params map[string]interface{} `json:"params,omitempty"`
	Chain  []ChainStep            `json:"chain,omitempty"`
}

func main() {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "GROQ_API_KEY is not set")
		os.Exit(1)
	}

	tools := loadTools()

	fmt.Println("Maria CLI — describe what you need to do")
	fmt.Println("Type 'exit' to quit.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		llmResp, err := askGroq(apiKey, input, tools)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}

		steps := llmResp.Chain
		if len(steps) == 0 {
			if llmResp.Tool == "" {
				fmt.Println("Could not determine which tool to use. Try rephrasing.")
				continue
			}
			steps = []ChainStep{{Tool: llmResp.Tool, Params: llmResp.Params}}
		}

		failed := false
		for i, step := range steps {
			if len(steps) > 1 {
				fmt.Printf("\n--- Step %d/%d: %s ---\n", i+1, len(steps), step.Tool)
			}

			toolDef, ok := findTool(tools, step.Tool)
			if !ok {
				fmt.Printf("Tool '%s' not found.\n", step.Tool)
				failed = true
				break
			}

			if err := runStep(toolDef, step.Params); err != nil {
				fmt.Fprintf(os.Stderr, "Error in step %d: %v\n", i+1, err)
				failed = true
				break
			}
		}

		if failed {
			fmt.Println("Chain stopped.")
		}
	}
}

func runStep(def ToolDef, params map[string]interface{}) error {
	cmdLine, err := buildCommand(def, params)
	if err != nil {
		return fmt.Errorf("building command: %w", err)
	}

	if def.DangerLevel == "confirm" {
		fmt.Printf("Command: %s\n", cmdLine)
		fmt.Print("Execute? [Enter] Yes / [n] No: ")
		confirm, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		confirm = strings.TrimSpace(confirm)
		if confirm == "n" || confirm == "N" || confirm == "no" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	return executeWithRetry(cmdLine, def, params)
}

func baseDir() string {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error getting executable path:", err)
		os.Exit(1)
	}
	return filepath.Dir(exe)
}

func loadTools() []ToolDef {
	indexPath := filepath.Join(baseDir(), "..", "tools", "tools.yaml")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to read tools/tools.yaml:", err)
		os.Exit(1)
	}
	var index ToolIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		fmt.Fprintln(os.Stderr, "Error parsing tools/tools.yaml:", err)
		os.Exit(1)
	}

	var tools []ToolDef
	for _, entry := range index.Tools {
		toolDir := filepath.Join(baseDir(), "..", "tools", filepath.Dir(entry.Path))
		toolPath := filepath.Join(toolDir, filepath.Base(entry.Path))
		defData, err := os.ReadFile(toolPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", toolPath, err)
			continue
		}
		var def ToolDef
		if err := yaml.Unmarshal(defData, &def); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", toolPath, err)
			continue
		}
		def.resolvedExec = resolveExec(def.Executable, toolDir)
		tools = append(tools, def)
	}
	return tools
}

func findTool(tools []ToolDef, name string) (ToolDef, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolDef{}, false
}

func resolveExec(executable, toolDir string) string {
	if strings.Contains(executable, "/") {
		return filepath.Join(toolDir, executable)
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return filepath.Join(toolDir, executable)
	}
	return path
}

func buildCommand(def ToolDef, params map[string]interface{}) (string, error) {
	parts := []string{def.resolvedExec}

	type posArg struct {
		index int
		value string
	}
	var positional []posArg
	var flags []string

	for _, arg := range def.Arguments {
		val, exists := params[arg.Name]
		if !exists {
			if arg.Required {
				return "", fmt.Errorf("required argument '%s' not provided", arg.Name)
			}
			if arg.Default != nil {
				switch d := arg.Default.(type) {
				case bool:
					if d {
						val = true
					} else {
						continue
					}
				case string:
					if d != "" {
						val = d
					} else {
						continue
					}
				default:
					continue
				}
			}
			continue
		}

		if arg.Positional > 0 || (arg.Positional == 0 && arg.Flag == "") {
			positional = append(positional, posArg{arg.Positional, fmt.Sprintf("%v", val)})
		} else if arg.Flag != "" {
			switch v := val.(type) {
			case bool:
				if v {
					flags = append(flags, arg.Flag)
				}
			case string:
				if v != "" {
					flags = append(flags, arg.Flag, v)
				}
			default:
				s := fmt.Sprintf("%v", v)
				if s != "" {
					flags = append(flags, arg.Flag, s)
				}
			}
		}
	}

	sort.Slice(positional, func(i, j int) bool {
		return positional[i].index < positional[j].index
	})
	for _, p := range positional {
		parts = append(parts, p.value)
	}
	parts = append(parts, flags...)

	return strings.Join(parts, " "), nil
}

func executeWithRetry(cmdLine string, def ToolDef, params map[string]interface{}) error {
	for attempt := 0; attempt < 2; attempt++ {
		fmt.Printf("Running: %s\n", cmdLine)

		var stderr bytes.Buffer
		cmd := exec.Command("bash", "-c", cmdLine)
		cmd.Stdout = os.Stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err == nil {
			return nil
		}

		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			fmt.Fprintln(os.Stderr, errMsg)
		}

		if attempt == 0 && tryRecoverDestDir(def, params) {
			fmt.Println("Retrying...")
		} else {
			return fmt.Errorf("%s", errMsg)
		}
	}
	return nil
}

func tryRecoverDestDir(def ToolDef, params map[string]interface{}) bool {
	destPath := findDestPath(def, params)
	if destPath == "" {
		return false
	}

	parent := filepath.Dir(destPath)

	parentExists := false
	if _, err := os.Stat(parent); err == nil {
		parentExists = true
	}

	var createDir string
	if parentExists {
		createDir = destPath
	} else {
		createDir = parent
	}

	_, statErr := os.Stat(createDir)
	if statErr == nil {
		return false
	}
	if !os.IsNotExist(statErr) {
		return false
	}

	fmt.Printf("\nDirectory '%s' does not exist.\n", createDir)
	fmt.Print("Create it? [Enter] Yes / [n] No: ")
	resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	resp = strings.TrimSpace(resp)

	if resp == "n" || resp == "N" || resp == "no" {
		fmt.Println("Cancelled.")
		return false
	}

	if err := os.MkdirAll(createDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
		return false
	}
	fmt.Printf("Directory '%s' created.\n", createDir)
	return true
}

func findDestPath(def ToolDef, params map[string]interface{}) string {
	args := def.Arguments

	lastPos := -1
	var lastPosName string
	for _, a := range args {
		if a.Flag == "" && a.Positional >= lastPos {
			lastPos = a.Positional
			lastPosName = a.Name
		}
	}
	if lastPosName != "" {
		if v, ok := params[lastPosName]; ok {
			return expandPath(fmt.Sprintf("%v", v))
		}
	}

	for _, name := range []string{"dest", "destination", "output"} {
		if v, ok := params[name]; ok {
			return expandPath(fmt.Sprintf("%v", v))
		}
	}

	for _, a := range args {
		if a.Name == "dest" || a.Name == "destination" || a.Name == "output" {
			if v, ok := params[a.Name]; ok {
				return expandPath(fmt.Sprintf("%v", v))
			}
		}
	}

	return ""
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~") {
		p = strings.Replace(p, "~", os.Getenv("HOME"), 1)
	}
	return p
}

func systemContext() string {
	locale := os.Getenv("LANG")
	if locale == "" {
		locale = "C (default)"
	}

	parts := []string{fmt.Sprintf("System locale: %s", locale)}
	parts = append(parts, "Actual user directory paths (use these paths in arguments):")

	xdgFile := filepath.Join(os.Getenv("HOME"), ".config", "user-dirs.dirs")
	data, err := os.ReadFile(xdgFile)
	if err != nil {
		parts = append(parts, fmt.Sprintf("  $HOME = %s", os.Getenv("HOME")))
		return strings.Join(parts, "\n")
	}

	re := regexp.MustCompile(`^XDG_(\w+)_DIR\s*=\s*"([^"]+)"`)
	for _, line := range strings.Split(string(data), "\n") {
		m := re.FindStringSubmatch(line)
		if len(m) < 3 {
			continue
		}
		name := strings.ToLower(m[1])
		path := strings.ReplaceAll(m[2], "$HOME", os.Getenv("HOME"))
		dirName := filepath.Base(path)
		parts = append(parts, fmt.Sprintf("  %-20s %s", name+":", path))
		_ = dirName
	}

	return strings.Join(parts, "\n")
}

func buildPrompt(userInput string, tools []ToolDef) string {
	var sb strings.Builder
	sb.WriteString("You are a system tool selector. Given the user's message, choose the most appropriate tool and fill in its parameters.\n\n")
	sb.WriteString("Respond with EXACTLY ONE valid JSON object, no explanations, no extra text.\n")
	sb.WriteString("For a single tool:\n")
	sb.WriteString("{\"tool\": \"tool_name\", \"params\": {\"param1\": \"value1\"}}\n")
	sb.WriteString("For multiple tools in sequence, use the chain format (do NOT output separate JSON objects):\n")
	sb.WriteString("{\"chain\": [{\"tool\": \"tool1\", \"params\": {\"p1\": \"v1\"}}, {\"tool\": \"tool2\", \"params\": {\"p1\": \"v1\"}}]}\n\n")
	sb.WriteString("Available tools:\n\n")

	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("=== %s ===\n", t.Name))
		sb.WriteString(fmt.Sprintf("Description: %s\n", t.Description))
		sb.WriteString("Arguments:\n")
		for _, a := range t.Arguments {
			req := ""
			if a.Required {
				req = " (required)"
			}
			pos := ""
			if a.Flag == "" {
				pos = fmt.Sprintf(" [positional %d]", a.Positional)
			}
			sb.WriteString(fmt.Sprintf("  - %s%s: %s%s\n", a.Name, pos, a.Description, req))
		}
		if len(t.Examples) > 0 {
			sb.WriteString("Examples:\n")
			for _, e := range t.Examples {
				sb.WriteString(fmt.Sprintf("  %s\n", e))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("System context:\n")
	sb.WriteString(systemContext())
	sb.WriteString("\n\n")

	sb.WriteString("User message: ")
	sb.WriteString(userInput)
	return sb.String()
}

func askGroq(apiKey, userInput string, tools []ToolDef) (*LLMResponse, error) {
	prompt := buildPrompt(userInput, tools)

	body := map[string]interface{}{
		"model": "llama-3.3-70b-versatile",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.1,
	}

	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Groq: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Groq returned %d: %s", resp.StatusCode, string(respBytes))
	}

	var groqResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &groqResp); err != nil {
		return nil, fmt.Errorf("parsing Groq response: %w", err)
	}

	if len(groqResp.Choices) == 0 {
		return nil, fmt.Errorf("Groq returned no choices")
	}

	raw := strings.TrimSpace(groqResp.Choices[0].Message.Content)
	var llmResp LLMResponse

	content := cleanJSON(raw)
	err = json.Unmarshal([]byte(content), &llmResp)
	if err == nil && (llmResp.Tool != "" || len(llmResp.Chain) > 0) {
		return &llmResp, nil
	}

	objects := extractJSONObjects(raw)
	if len(objects) > 1 {
		var steps []ChainStep
		for _, obj := range objects {
			var step struct {
				Tool   string                 `json:"tool"`
				Params map[string]interface{} `json:"params"`
			}
			if err := json.Unmarshal([]byte(obj), &step); err == nil && step.Tool != "" {
				steps = append(steps, ChainStep{Tool: step.Tool, Params: step.Params})
			}
		}
		if len(steps) > 0 {
			llmResp.Chain = steps
			return &llmResp, nil
		}
	}

	return nil, fmt.Errorf("parsing JSON: %w\nContent: %s", err, raw)

	return &llmResp, nil
}

func cleanJSON(s string) string {
	if idx := strings.Index(s, "{"); idx != -1 {
		s = s[idx:]
	}
	if idx := strings.LastIndex(s, "}"); idx != -1 {
		s = s[:idx+1]
	}
	return s
}

func extractJSONObjects(s string) []string {
	var objs []string
	depth := 0
	start := -1
	for i, c := range s {
		switch c {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				objs = append(objs, s[start:i+1])
				start = -1
			}
		}
	}
	return objs
}
