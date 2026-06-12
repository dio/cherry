package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chzyer/readline"
	cherry "github.com/dio/cherry"
	"github.com/dio/cherry/example/source"
	"github.com/dio/cherry/example/transform"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printFixtureTree(source.ExampleFixture())
		return nil
	}
	switch args[0] {
	case "pack":
		return runPack(args[1:])
	case "repl":
		return runREPL(args[1:])
	case "stress-pack":
		return runStressPack(args[1:])
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		fixture, err := source.LoadFixtureYAML(args[0])
		if err != nil {
			return err
		}
		printFixtureTree(fixture)
		return nil
	}
}

func printHelp() {
	fmt.Println("usage:")
	fmt.Println("  go run ./example")
	fmt.Println("  go run ./example [fixture.yaml]")
	fmt.Println("  go run ./example pack <workspace|project> <id> <fixture.yaml> [out.zst]")
	fmt.Println("  go run ./example repl <cherry.zst>")
	fmt.Println("  go run ./example stress-pack <principals-per-scope> <queries> [scopes]")
}

func runPack(args []string) error {
	if len(args) != 3 && len(args) != 4 {
		return fmt.Errorf("usage: pack <workspace|project> <id> <fixture.yaml> [out.zst]")
	}
	scopeKind := transform.ScopeKind(args[0])
	if scopeKind != transform.ScopeKindWorkspace && scopeKind != transform.ScopeKindProject {
		return fmt.Errorf("invalid scope kind %q; want workspace or project", args[0])
	}
	scopeID := args[1]
	fixture, err := source.LoadFixtureYAML(args[2])
	if err != nil {
		return err
	}
	outPath := fmt.Sprintf("%s-%s.pack.zst", scopeKind, scopeID)
	if len(args) == 4 {
		outPath = args[3]
	}

	result, err := transform.BuildPackInput(fixture, transform.Selection{Kind: scopeKind, ID: scopeID})
	if err != nil {
		return err
	}
	blob, manifest, err := cherry.BuildWithManifest(result.Input)
	if err != nil {
		return err
	}
	bundle := cherry.NewBundle(string(scopeKind), scopeID, result.Scopes, blob, manifest)
	compressed, err := cherry.EncodeBundleZstd(bundle)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, compressed, 0644); err != nil {
		return err
	}
	fmt.Printf("wrote %s scope=%s:%s scopes=%s raw_bytes=%d\n", outPath, scopeKind, scopeID, strings.Join(result.Scopes, ","), len(blob))
	return nil
}

func runREPL(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: repl <cherry.zst>")
	}
	opened, err := loadPack(args[0])
	if err != nil {
		return err
	}
	session := replSession{
		path:  args[0],
		pack:  opened,
		scope: defaultScope(opened.Metadata.Scopes),
	}
	fmt.Printf("loaded pack scope=%s:%s scopes=%s raw_bytes=%d\n", opened.Metadata.ScopeKind, opened.Metadata.ScopeID, strings.Join(opened.Metadata.Scopes, ","), len(opened.Blob))
	if session.scope != "" {
		fmt.Printf("using scope %s\n", session.scope)
	}
	fmt.Println("commands: summary, scopes, use <scope>, llm [scope] <slug> <model>, mcp [scope] <path> [tool], inspect <metadata|principals|mcp|all>, reload, help, quit")

	rl, err := readline.NewEx(&readline.Config{Prompt: "cherry> "})
	if err != nil {
		return err
	}
	defer rl.Close()
	for {
		line, err := rl.Readline()
		if err == io.EOF || err == readline.ErrInterrupt {
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !session.handle(line) {
			return nil
		}
	}
}

type replSession struct {
	path  string
	pack  cherry.OpenedBundle
	scope string
}

func (s *replSession) handle(line string) bool {
	fields := strings.Fields(line)
	switch fields[0] {
	case "quit", "exit":
		return false
	case "help":
		printREPLHelp()
	case "summary":
		printPackSummary(s.pack, s.scope)
	case "scopes":
		printYAML(s.pack.Reader.ScopeIDs())
	case "use":
		if len(fields) != 2 {
			fmt.Println("usage: use <scope>")
			return true
		}
		if !contains(s.pack.Metadata.Scopes, fields[1]) {
			fmt.Printf("scope %q is not in this bundle; scopes=%s\n", fields[1], strings.Join(s.pack.Metadata.Scopes, ","))
			return true
		}
		s.scope = fields[1]
		fmt.Printf("using scope %s\n", s.scope)
	case "reload":
		opened, err := loadPack(s.path)
		if err != nil {
			fmt.Printf("reload failed: %v\n", err)
			return true
		}
		s.pack = opened
		if !contains(opened.Metadata.Scopes, s.scope) {
			s.scope = defaultScope(opened.Metadata.Scopes)
		}
		fmt.Printf("reloaded scopes=%s\n", strings.Join(opened.Metadata.Scopes, ","))
	case "inspect":
		s.inspect(fields)
	case "llm":
		s.llm(fields)
	case "mcp":
		s.mcp(fields)
	default:
		fmt.Printf("unknown command %q; type help\n", fields[0])
	}
	return true
}

func (s *replSession) inspect(fields []string) {
	if len(fields) != 2 {
		fmt.Println("usage: inspect <metadata|principals|mcp|all>")
		return
	}
	switch fields[1] {
	case "metadata":
		printYAML(s.pack.Metadata)
	case "principals":
		scope := s.activeScope()
		if scope == "" {
			return
		}
		routes, ok := s.pack.Reader.PrincipalRoutes(scope)
		if !ok {
			fmt.Printf("unknown scope %q\n", scope)
			return
		}
		printYAML(routes)
	case "mcp":
		scope := s.activeScope()
		if scope == "" {
			return
		}
		paths, ok := s.pack.Reader.MCPPaths(scope)
		if !ok {
			fmt.Printf("unknown scope %q\n", scope)
			return
		}
		printYAML(paths)
	case "all":
		scope := s.activeScope()
		if scope == "" {
			return
		}
		routes, _ := s.pack.Reader.PrincipalRoutes(scope)
		paths, _ := s.pack.Reader.MCPPaths(scope)
		printYAML(struct {
			Metadata   cherry.BundleMetadata   `yaml:"metadata"`
			Principals []cherry.PrincipalRoute `yaml:"principals"`
			MCP        []cherry.MCPPath        `yaml:"mcp"`
		}{s.pack.Metadata, routes, paths})
	default:
		fmt.Printf("unknown inspect area %q\n", fields[1])
	}
}

func (s *replSession) llm(fields []string) {
	if len(fields) != 3 && len(fields) != 4 {
		fmt.Println("usage: llm [scope] <principal-slug> <model>")
		return
	}
	scope, offset := s.scopeAndOffset(fields, 3)
	if scope == "" {
		return
	}
	principalSlug := fields[offset]
	requestedModel := fields[offset+1]
	ids, ok := s.pack.Reader.ResolveLLMIDs(scope, principalSlug, requestedModel)
	if !ok {
		fmt.Printf("rejected: no LLM route for scope=%s principal=%s model=%s\n", scope, principalSlug, requestedModel)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("principal: %s\n", principalSlug)
	fmt.Printf("requested_model: %s\n", requestedModel)
	fmt.Printf("target:\n")
	fmt.Printf(
		"  provider=%s kind=%s model=%s model_name=%s endpoint=%s secret_ref=%s\n",
		s.pack.Reader.String(ids.ProviderSID),
		s.pack.Reader.String(ids.KindSID),
		s.pack.Reader.String(ids.ModelSID),
		s.pack.Reader.String(ids.ModelNameSID),
		s.pack.Reader.String(ids.EndpointSID),
		s.pack.Reader.String(ids.SecretSID),
	)
	fmt.Printf("rate_limit: usd_per_day=%.2f rpm=%d on_exceed=%s\n", float64(ids.Rate.USDPerDayCents)/100, ids.Rate.RPM, s.pack.Reader.String(ids.Rate.OnExceedSID))
}

func (s *replSession) mcp(fields []string) {
	if len(fields) < 2 || len(fields) > 4 {
		fmt.Println("usage: mcp [scope] <path> [tool]")
		return
	}
	scope := s.scope
	pathOffset := 1
	if len(fields) >= 3 && contains(s.pack.Metadata.Scopes, fields[1]) {
		scope = fields[1]
		pathOffset = 2
	}
	if scope == "" {
		fmt.Println("no active scope; run use <scope>")
		return
	}
	path := fields[pathOffset]
	if len(fields) > pathOffset+1 {
		toolName := fields[pathOffset+1]
		tool, ok := s.pack.Reader.ResolveMCPToolIDs(scope, path, toolName)
		if !ok {
			fmt.Printf("rejected: no MCP tool for scope=%s path=%s tool=%s\n", scope, path, toolName)
			return
		}
		fmt.Printf("scope: %s\n", scope)
		fmt.Printf("path: %s\n", path)
		fmt.Printf("tool: %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n",
			toolName,
			s.pack.Reader.String(tool.ServerSID),
			s.pack.Reader.String(tool.ServerEndpointSID),
			s.pack.Reader.String(tool.ToolSID),
			s.pack.Reader.String(tool.AuthTypeSID),
			s.pack.Reader.String(tool.SecretSID),
		)
		return
	}
	result, ok := s.pack.Reader.ResolveMCP(scope, path)
	if !ok {
		fmt.Printf("rejected: no MCP path for scope=%s path=%s\n", scope, path)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("path: %s\n", result.Path)
	fmt.Println("tools:")
	for _, tool := range result.Tools {
		fmt.Printf("  %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n", tool.ExposedName, tool.Server, tool.ServerEndpoint, tool.Tool, tool.AuthType, tool.SecretRef)
	}
}

func (s *replSession) activeScope() string {
	if s.scope == "" {
		fmt.Println("no active scope; run use <scope>")
	}
	return s.scope
}

func (s *replSession) scopeAndOffset(fields []string, noScopeLen int) (string, int) {
	if len(fields) == noScopeLen {
		if s.scope == "" {
			fmt.Println("no active scope; run use <scope>")
			return "", 0
		}
		return s.scope, 1
	}
	if !contains(s.pack.Metadata.Scopes, fields[1]) {
		fmt.Printf("scope %q is not in this bundle\n", fields[1])
		return "", 0
	}
	return fields[1], 2
}

func printREPLHelp() {
	fmt.Println("commands:")
	fmt.Println("  summary                         print bundle metadata")
	fmt.Println("  scopes                          list scopes in the loaded bundle")
	fmt.Println("  use <scope>                     select the active enforcement scope")
	fmt.Println("  llm [scope] <slug> <model>      resolve an LLM request")
	fmt.Println("  mcp [scope] <path> [tool]       resolve or list MCP tools for a path")
	fmt.Println("  inspect metadata                print bundle metadata")
	fmt.Println("  inspect principals              list principal/model routes for active scope")
	fmt.Println("  inspect mcp                     list MCP paths and tool bindings for active scope")
	fmt.Println("  inspect all                     print all inspectable data for active scope")
	fmt.Println("  reload                          reload the pack file")
	fmt.Println("  quit                            exit")
}

func loadPack(path string) (cherry.OpenedBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cherry.OpenedBundle{}, err
	}
	return cherry.OpenBundleZstd(data)
}

func defaultScope(scopes []string) string {
	if len(scopes) == 1 {
		return scopes[0]
	}
	return ""
}

func printPackSummary(opened cherry.OpenedBundle, activeScope string) {
	fmt.Printf("pack: %s scope=%s:%s\n", opened.Metadata.FormatVersion, opened.Metadata.ScopeKind, opened.Metadata.ScopeID)
	fmt.Printf("  scopes: %s\n", strings.Join(opened.Metadata.Scopes, ", "))
	if activeScope != "" {
		fmt.Printf("  active_scope: %s\n", activeScope)
	}
	fmt.Printf("  raw_bytes: %d\n", len(opened.Blob))
	fmt.Printf("  manifest_version: %d\n", opened.Metadata.PackManifest.FormatVersion)
	fmt.Printf("  manifest_checksum: %d\n", opened.Metadata.PackManifest.Checksum)
}

func runStressPack(args []string) error {
	if len(args) != 2 && len(args) != 3 {
		return fmt.Errorf("usage: stress-pack <principals-per-scope> <queries> [scopes]")
	}
	principals, err := strconv.Atoi(args[0])
	if err != nil || principals <= 0 {
		return fmt.Errorf("invalid principals-per-scope %q", args[0])
	}
	queries, err := strconv.Atoi(args[1])
	if err != nil || queries <= 0 {
		return fmt.Errorf("invalid queries %q", args[1])
	}
	scopes := 1
	if len(args) == 3 {
		scopes, err = strconv.Atoi(args[2])
		if err != nil || scopes <= 0 {
			return fmt.Errorf("invalid scopes %q", args[2])
		}
	}

	runtime.GC()
	beforeBuild := heapAlloc()
	start := time.Now()
	input := buildStressInput(scopes, principals)
	blob, manifest, err := cherry.BuildWithManifest(input)
	if err != nil {
		return err
	}
	buildElapsed := time.Since(start)
	afterBuild := heapAlloc()

	runtime.GC()
	beforeOpen := heapAlloc()
	reader, err := cherry.OpenWithManifest(blob, manifest)
	if err != nil {
		return err
	}
	afterOpen := heapAlloc()

	latencies := make([]int64, 0, queries)
	queryStart := time.Now()
	var found int
	for i := 0; i < queries; i++ {
		scopeID := "workspace" + strconv.Itoa(i%scopes+1)
		principalID := i%principals + 1
		principalSlug := fmt.Sprintf("slug:%d:%d", i%scopes+1, principalID)
		lookupStart := time.Now()
		_, ok := reader.ResolveLLMIDs(scopeID, principalSlug, "gpt-4o-mini")
		latencies = append(latencies, time.Since(lookupStart).Nanoseconds())
		if ok {
			found++
		}
	}
	queryElapsed := time.Since(queryStart)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	fmt.Printf("stress_pack:\n")
	fmt.Printf("  scopes: %d\n", scopes)
	fmt.Printf("  principals_per_scope: %d\n", principals)
	fmt.Printf("  total_principals: %d\n", scopes*principals)
	fmt.Printf("  queries: %d\n", queries)
	fmt.Printf("  found: %d\n", found)
	fmt.Printf("  build_time_ms: %d\n", buildElapsed.Milliseconds())
	fmt.Printf("  raw_pack_bytes: %d\n", len(blob))
	fmt.Printf("  manifest_version: %d\n", manifest.FormatVersion)
	fmt.Printf("  manifest_checksum: %d\n", manifest.Checksum)
	fmt.Printf("  heap_build_delta_bytes: %d\n", int64(afterBuild)-int64(beforeBuild))
	fmt.Printf("  heap_open_delta_bytes: %d\n", int64(afterOpen)-int64(beforeOpen))
	fmt.Printf("  reader_blob_bytes: %d\n", reader.BlobSize())
	fmt.Printf("  query_total_ms: %d\n", queryElapsed.Milliseconds())
	fmt.Printf("  query_avg_ns: %d\n", queryElapsed.Nanoseconds()/int64(queries))
	fmt.Printf("  query_p50_ns: %d\n", percentile(latencies, 50))
	fmt.Printf("  query_p95_ns: %d\n", percentile(latencies, 95))
	fmt.Printf("  query_p99_ns: %d\n", percentile(latencies, 99))
	return nil
}

func buildStressInput(scopeCount int, principalsPerScope int) cherry.Input {
	providers := []cherry.Provider{{ID: "openai", Kind: "openai", Endpoint: "https://api.openai.com", SecretRef: "env://OPENAI_API_KEY"}}
	models := []cherry.Model{{ID: "gpt-4o-mini", Provider: "openai", Name: "gpt-4o-mini"}}
	mcpServers := []cherry.MCPServer{{ID: "github", Endpoint: "https://api.github.com"}, {ID: "kiwi", Endpoint: "https://mcp.kiwi.com"}}
	scopes := make([]cherry.Scope, 0, scopeCount)
	for scopeIndex := 0; scopeIndex < scopeCount; scopeIndex++ {
		scope := cherry.Scope{
			ID:         fmt.Sprintf("workspace%d", scopeIndex+1),
			Principals: make([]cherry.Principal, 0, principalsPerScope),
			MCPProfiles: []cherry.MCPProfile{{
				Path: "profile-dev-tools",
				Tools: []cherry.MCPToolBinding{
					{ExposedName: "github__list-repos", Server: "github", Tool: "list-repos"},
					{ExposedName: "kiwi__search-flight", Server: "kiwi", Tool: "search-flight"},
				},
			}},
		}
		for principalIndex := 0; principalIndex < principalsPerScope; principalIndex++ {
			scope.Principals = append(scope.Principals, cherry.Principal{
				Slug:  fmt.Sprintf("slug:%d:%d", scopeIndex+1, principalIndex+1),
				Route: cherry.RoutePlan{Provider: "openai", Model: "gpt-4o-mini"},
				Rate:  cherry.RatePolicy{USDPerDayCents: 50000, RPM: 300, OnExceed: "reject"},
			})
		}
		scopes = append(scopes, scope)
	}
	return cherry.Input{Providers: providers, Models: models, MCPServers: mcpServers, Scopes: scopes}
}

func printFixtureTree(fixture source.Fixture) {
	root := treeNode{Label: "org " + fixture.OrgID}
	for _, project := range fixture.Projects {
		projectNode := treeNode{Label: "project " + project.ID}
		for _, workspace := range workspacesForProject(fixture, project.ID) {
			workspaceNode := treeNode{Label: "workspace " + workspace.ID}
			for _, key := range fixture.Keys {
				if transform.KeySelectableInWorkspace(fixture, key, workspace) {
					workspaceNode.Children = append(workspaceNode.Children, treeNode{
						Label: fmt.Sprintf("key %s slug=%s owner=%s scope=%s tags=%s", key.ID, key.Slug, key.UserID, key.Scope, strings.Join(key.TagIDs, ",")),
					})
				}
			}
			for _, profile := range fixture.Profiles {
				if profile.WorkspaceID == workspace.ID {
					workspaceNode.Children = append(workspaceNode.Children, treeNode{
						Label: fmt.Sprintf("profile %s path=%s slug=%s", profile.ID, profile.Path, profile.Slug),
					})
				}
			}
			projectNode.Children = append(projectNode.Children, workspaceNode)
		}
		root.Children = append(root.Children, projectNode)
	}
	llmNode := treeNode{Label: "llm"}
	for _, model := range fixture.Models {
		llmNode.Children = append(llmNode.Children, treeNode{Label: fmt.Sprintf("model %s provider=%s", model.ID, model.Provider)})
	}
	root.Children = append(root.Children, llmNode)
	mcpNode := treeNode{Label: "mcp"}
	for _, server := range fixture.MCPServers {
		mcpNode.Children = append(mcpNode.Children, treeNode{Label: fmt.Sprintf("server %s tools=%s", server.ID, strings.Join(server.Tools, ","))})
	}
	root.Children = append(root.Children, mcpNode)
	printTree(root)
}

type treeNode struct {
	Label    string
	Children []treeNode
}

func printTree(root treeNode) {
	fmt.Println(root.Label)
	for i, child := range root.Children {
		printTreeNode(child, "", i == len(root.Children)-1)
	}
}

func printTreeNode(node treeNode, prefix string, last bool) {
	connector := "|- "
	nextPrefix := prefix + "|  "
	if last {
		connector = "`- "
		nextPrefix = prefix + "   "
	}
	fmt.Println(prefix + connector + node.Label)
	for i, child := range node.Children {
		printTreeNode(child, nextPrefix, i == len(node.Children)-1)
	}
}

func workspacesForProject(fixture source.Fixture, projectID string) []source.Workspace {
	workspaces := []source.Workspace{}
	for _, workspace := range fixture.Workspaces {
		if workspace.ProjectID == projectID {
			workspaces = append(workspaces, workspace)
		}
	}
	return workspaces
}

func printYAML(value any) {
	data, err := yaml.Marshal(value)
	if err != nil {
		fmt.Printf("marshal yaml: %v\n", err)
		return
	}
	fmt.Print(string(data))
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func heapAlloc() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func percentile(sortedValues []int64, p int) int64 {
	if len(sortedValues) == 0 {
		return 0
	}
	index := (len(sortedValues)*p + 99) / 100
	if index <= 0 {
		index = 1
	}
	if index > len(sortedValues) {
		index = len(sortedValues)
	}
	return sortedValues[index-1]
}
