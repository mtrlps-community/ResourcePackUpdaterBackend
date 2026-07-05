package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	expectedUserAgentPrefix  = "ResourcePackUpdater/"
	expectedUserAgentExample = "ResourcePackUpdater/<mod_version> +https://www.zbx1425.cn"
)

var ignoredNames = map[string]bool{
	"updater_hash_cache.bin": true,
}

type publisherConfig struct {
	Source            string   `json:"source"`
	Src               string   `json:"src"`
	InputDir          string   `json:"inputDir"`
	Inputs            []string `json:"inputs"`
	Destination       string   `json:"destination"`
	Dst               string   `json:"dst"`
	PublicBaseURL     string   `json:"publicBaseUrl"`
	BaseURL           string   `json:"baseUrl"`
	SourceName        string   `json:"sourceName"`
	LocalPackName     string   `json:"localPackName"`
	WriteClientConfig *bool    `json:"writeClientConfig"`
	ServerLock        string   `json:"serverLock"`
	ServerLockAlt     string   `json:"server_lock"`
	Encrypt           bool     `json:"encrypt"`
	Clean             *bool    `json:"clean"`
	PackDescription   string   `json:"packDescription"`
	PackFormat        int      `json:"packFormat"`
}

type fileEntry struct {
	RelPath string
	SHA1    string
	Size    int64
	MTime   int64
}

type metadataFile struct {
	SHA1  string `json:"sha1"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

type metadata struct {
	Version     int  `json:"version"`
	Encrypt     bool `json:"encrypt"`
	FileContent struct {
		Dirs  map[string]bool         `json:"dirs"`
		Files map[string]metadataFile `json:"files"`
	} `json:"file_content"`
}

func main() {
	configArg := flag.String("config", "", "publisher JSON config file")
	sourceArg := flag.String("source", "", "single source resource pack directory or zip")
	srcArg := flag.String("src", "", "alias for -source")
	inputDirArg := flag.String("input-dir", "", "directory containing resource pack zips/folders")
	dstArg := flag.String("dst", "", "destination publish root")
	serverLockArg := flag.String("server-lock", "", "optional zbx_rpu_server_lock value")
	publicBaseURLArg := flag.String("public-base-url", "", "public base URL for generated client_config.json")
	sourceNameArg := flag.String("source-name", "", "source name for generated client_config.json")
	localPackNameArg := flag.String("local-pack-name", "", "localPackName for generated client_config.json")
	encryptArg := flag.Bool("encrypt", false, "mark metadata as encrypted")
	noCleanArg := flag.Bool("no-clean", false, "do not clear generated output first")
	noClientConfigArg := flag.Bool("no-client-config", false, "do not generate client_config.json")
	flag.Parse()

	cfg, err := loadConfig(*configArg)
	must(err)

	sourceValue := firstNonEmpty(*sourceArg, *srcArg, cfg.Source, cfg.Src)
	inputDirValue := firstNonEmpty(*inputDirArg, cfg.InputDir)
	dstValue := firstNonEmpty(*dstArg, cfg.Destination, cfg.Dst)
	serverLockValue := firstNonEmpty(*serverLockArg, cfg.ServerLock, cfg.ServerLockAlt)
	publicBaseURLValue := firstNonEmpty(*publicBaseURLArg, cfg.PublicBaseURL, cfg.BaseURL)
	sourceNameValue := firstNonEmpty(*sourceNameArg, cfg.SourceName, "Main")
	localPackNameValue := firstNonEmpty(*localPackNameArg, cfg.LocalPackName, "SyncedPack")
	packDescriptionValue := firstNonEmpty(cfg.PackDescription, "Server Resource Pack")
	packFormatValue := cfg.PackFormat
	if packFormatValue == 0 {
		packFormatValue = 22
	}

	cleanValue := true
	if cfg.Clean != nil {
		cleanValue = *cfg.Clean
	}
	if *noCleanArg {
		cleanValue = false
	}

	writeClientConfigValue := publicBaseURLValue != ""
	if cfg.WriteClientConfig != nil {
		writeClientConfigValue = publicBaseURLValue != "" && *cfg.WriteClientConfig
	}
	if *noClientConfigArg {
		writeClientConfigValue = false
	}

	if dstValue == "" {
		flag.Usage()
		fail("destination is required")
	}

	dst, err := filepath.Abs(dstValue)
	must(err)
	dist := filepath.Join(dst, "dist")

	inputs, err := collectInputs(inputDirValue, sourceValue, cfg.Inputs)
	must(err)
	if len(inputs) == 0 {
		fail("no resource pack inputs found")
	}

	must(os.MkdirAll(dst, 0o755))
	if cleanValue {
		must(cleanOutput(dst, dist, writeClientConfigValue))
	}
	must(os.MkdirAll(dist, 0o755))

	for _, input := range inputs {
		fmt.Printf("Merging %s\n", input)
		must(mergeInput(input, dist))
	}

	must(ensurePackMeta(filepath.Join(dist, "pack.mcmeta"), packFormatValue, packDescriptionValue))
	if serverLockValue != "" {
		must(patchPackMetaWithServerLock(filepath.Join(dist, "pack.mcmeta"), serverLockValue))
	}

	dirs, files, err := buildMetadata(dist)
	must(err)
	must(writeMetadataJSON(dst, cfg.Encrypt || *encryptArg, dirs, files))
	dirSHA1, err := buildDirChecksum(dirs, files)
	must(err)
	must(writeMetadataSHA1(dst, cfg.Encrypt || *encryptArg, dirSHA1))
	if writeClientConfigValue {
		must(writeClientConfig(dst, publicBaseURLValue, sourceNameValue, localPackNameValue))
	}

	fmt.Printf("Published %d files to %s\n", len(files), dist)
	fmt.Printf("metadata.sha1: %s\n", dirSHA1)
	if writeClientConfigValue {
		fmt.Printf("client_config.json baseUrl: %s\n", strings.TrimRight(publicBaseURLValue, "/"))
	}
	fmt.Printf("Expected User-Agent prefix: %s\n", expectedUserAgentPrefix)
	fmt.Printf("Expected User-Agent example: %s\n", expectedUserAgentExample)
	if serverLockValue != "" {
		fmt.Println("Server lock injected into dist/pack.mcmeta")
	}
}

func loadConfig(configPath string) (publisherConfig, error) {
	var cfg publisherConfig
	if configPath == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return cfg, err
	}
	data = trimUTF8BOM(data)
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func collectInputs(inputDir, source string, configuredInputs []string) ([]string, error) {
	var inputs []string
	if inputDir != "" {
		entries, err := os.ReadDir(inputDir)
		if err != nil {
			return nil, err
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			fullPath := filepath.Join(inputDir, name)
			if entry.IsDir() || strings.HasSuffix(strings.ToLower(name), ".zip") {
				inputs = append(inputs, fullPath)
			}
		}
	}
	for _, input := range configuredInputs {
		if input != "" {
			inputs = append(inputs, input)
		}
	}
	if inputDir == "" && len(configuredInputs) == 0 && source != "" {
		inputs = append(inputs, source)
	}
	for i, input := range inputs {
		abs, err := filepath.Abs(input)
		if err != nil {
			return nil, err
		}
		inputs[i] = abs
	}
	return inputs, nil
}

func mergeInput(input, dist string) error {
	info, err := os.Stat(input)
	if err != nil {
		return err
	}
	if info.IsDir() {
		root, err := resolvePackDirectoryRoot(input)
		if err != nil {
			return err
		}
		return mergeDirectory(root, dist)
	}
	if strings.HasSuffix(strings.ToLower(input), ".zip") {
		return mergeZip(input, dist)
	}
	return fmt.Errorf("unsupported input: %s", input)
}

func resolvePackDirectoryRoot(input string) (string, error) {
	if fileExists(filepath.Join(input, "pack.mcmeta")) {
		return input, nil
	}
	entries, err := os.ReadDir(input)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(input, entry.Name())
		if fileExists(filepath.Join(candidate, "pack.mcmeta")) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return input, nil
}

func mergeDirectory(root, dist string) error {
	return filepath.WalkDir(root, func(currentPath string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "__pycache__" {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(root, currentPath)
			if err != nil {
				return err
			}
			if rel == "." {
				rel = ""
			}
			outPath, err := safeJoin(dist, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			return os.MkdirAll(outPath, 0o755)
		}
		if ignoredNames[name] {
			return nil
		}
		rel, err := filepath.Rel(root, currentPath)
		if err != nil {
			return err
		}
		outPath, err := safeJoin(dist, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		return copyFile(currentPath, outPath)
	})
}

func mergeZip(zipPath, dist string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	rootPrefix, err := detectZipPackRoot(reader.File)
	if err != nil {
		return fmt.Errorf("%s: %w", zipPath, err)
	}

	for _, f := range reader.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if rootPrefix != "" {
			if !strings.HasPrefix(name, rootPrefix) {
				continue
			}
			name = strings.TrimPrefix(name, rootPrefix)
		}
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			continue
		}
		cleanName := path.Clean(name)
		if cleanName == "." || cleanName == "" {
			continue
		}
		if !isSafeRelativePath(cleanName) {
			return fmt.Errorf("unsafe zip entry path: %s", f.Name)
		}
		if ignoredNames[path.Base(cleanName)] {
			continue
		}
		outPath, err := safeJoin(dist, cleanName)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(outPath, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(f, outPath); err != nil {
			return err
		}
	}
	return nil
}

func detectZipPackRoot(files []*zip.File) (string, error) {
	candidates := map[string]bool{}
	for _, f := range files {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		cleanName := path.Clean(name)
		if cleanName == "." || cleanName == "" {
			continue
		}
		if !isSafeRelativePath(cleanName) {
			return "", fmt.Errorf("unsafe zip entry path: %s", f.Name)
		}
		if cleanName == "pack.mcmeta" {
			return "", nil
		}
		parts := strings.Split(cleanName, "/")
		if len(parts) == 2 && parts[1] == "pack.mcmeta" {
			candidates[parts[0]+"/"] = true
		}
	}
	if len(candidates) == 1 {
		for candidate := range candidates {
			return candidate, nil
		}
	}
	return "", nil
}

func extractZipFile(f *zip.File, dst string) error {
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()

	mode := f.Mode()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !f.Modified.IsZero() {
		if err := os.Chtimes(dst, f.Modified, f.Modified); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot set mtime on %s: %v\n", dst, err)
		}
	}
	return nil
}

func cleanOutput(dst, dist string, removeClientConfig bool) error {
	if err := os.RemoveAll(dist); err != nil {
		return err
	}
	names := []string{"metadata.json", "metadata.sha1"}
	if removeClientConfig {
		names = append(names, "client_config.json")
	}
	for _, name := range names {
		err := os.Remove(filepath.Join(dst, name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func ensurePackMeta(packMetaPath string, packFormat int, description string) error {
	if fileExists(packMetaPath) {
		return nil
	}
	payload := map[string]any{
		"pack": map[string]any{
			"pack_format": packFormat,
			"description": description,
		},
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(packMetaPath, data, 0o644)
}

func patchPackMetaWithServerLock(packMetaPath, serverLock string) error {
	data, err := os.ReadFile(packMetaPath)
	if err != nil {
		return err
	}
	data = trimUTF8BOM(data)

	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return err
	}
	obj["zbx_rpu_server_lock"] = serverLock

	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(packMetaPath, out, 0o644)
}

func buildMetadata(root string) ([]string, []fileEntry, error) {
	dirs := map[string]bool{"": true}
	var files []fileEntry

	err := filepath.WalkDir(root, func(currentPath string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "__pycache__" {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(root, currentPath)
			if err != nil {
				return err
			}
			if rel == "." {
				rel = ""
			}
			dirs[filepath.ToSlash(rel)] = true
			return nil
		}
		if ignoredNames[name] {
			return nil
		}
		rel, err := filepath.Rel(root, currentPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Stat(currentPath)
		if err != nil {
			return err
		}
		sum, err := sha1File(currentPath)
		if err != nil {
			return err
		}
		files = append(files, fileEntry{
			RelPath: rel,
			SHA1:    sum,
			Size:    info.Size(),
			MTime:   info.ModTime().UnixNano() / 1_000_000,
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	dirList := make([]string, 0, len(dirs))
	for dir := range dirs {
		dirList = append(dirList, dir)
	}
	sort.Strings(dirList)
	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})
	return dirList, files, nil
}

func writeMetadataJSON(dst string, encrypt bool, dirs []string, files []fileEntry) error {
	meta := metadata{Version: 2, Encrypt: encrypt}
	meta.FileContent.Dirs = make(map[string]bool, len(dirs))
	meta.FileContent.Files = make(map[string]metadataFile, len(files))
	for _, dir := range dirs {
		meta.FileContent.Dirs[dir] = true
	}
	for _, file := range files {
		meta.FileContent.Files[file.RelPath] = metadataFile{
			SHA1:  file.SHA1,
			Size:  file.Size,
			MTime: file.MTime,
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dst, "metadata.json"), data, 0o644)
}

func writeMetadataSHA1(dst string, encrypt bool, dirSHA1 string) error {
	var data []byte
	if encrypt {
		payload := map[string]any{
			"sha1":    dirSHA1,
			"encrypt": true,
		}
		out, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		data = append(out, '\n')
	} else {
		data = []byte(dirSHA1 + "\n")
	}
	return os.WriteFile(filepath.Join(dst, "metadata.sha1"), data, 0o644)
}

func writeClientConfig(dst, publicBaseURL, sourceName, localPackName string) error {
	payload := map[string]any{
		"sources": []map[string]any{
			{
				"name":       sourceName,
				"baseUrl":    strings.TrimRight(publicBaseURL, "/"),
				"hasDirHash": true,
				"hasArchive": false,
			},
		},
	}
	if localPackName != "" {
		payload["localPackName"] = localPackName
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dst, "client_config.json"), data, 0o644)
}

func buildDirChecksum(dirs []string, files []fileEntry) (string, error) {
	h := sha1.New()
	for _, dir := range dirs {
		h.Write([]byte(dir))
	}
	for _, file := range files {
		h.Write([]byte(file.RelPath))
		sum, err := hex.DecodeString(file.SHA1)
		if err != nil {
			return "", err
		}
		h.Write(sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot set mtime on %s: %v\n", dst, err)
	}
	return nil
}

func sha1File(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeJoin(base, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	cleanRel := path.Clean(strings.TrimPrefix(rel, "/"))
	if cleanRel == "." {
		cleanRel = ""
	}
	if !isSafeRelativePath(cleanRel) {
		return "", fmt.Errorf("unsafe relative path: %s", rel)
	}
	return filepath.Join(base, filepath.FromSlash(cleanRel)), nil
}

func isSafeRelativePath(rel string) bool {
	return rel == "" || (rel != ".." && !strings.HasPrefix(rel, "../") && !path.IsAbs(rel))
}

func fileExists(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && !info.IsDir()
}

func trimUTF8BOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf {
		return data[3:]
	}
	return data
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
