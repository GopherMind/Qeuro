package mcp

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// baseEnvNames is the allow-list of environment variables a server process
// inherits. The child's environment is built from scratch and this list is the
// only thing that gets copied — the opposite of taking the parent's environment
// and deleting known secrets, which fails the moment a new QEURO_* variable is
// added and nobody remembers to extend the deny-list. Provider keys are absent
// by construction (roadmap §4.8, .ai/AI.md:49).
//
// The list is not minimalism for its own sake: each entry is here because a
// realistic server breaks without it.
var baseEnvNames = []string{
	"PATH",           // finding node/python and whatever the server itself shells out to
	"HOME",           // config and cache directories on unix
	"LANG", "LC_ALL", // text decoding
	"TZ",

	// Windows: a process started with an empty environment fails in ways that
	// look like a broken server rather than a missing variable. SystemRoot is
	// needed for socket initialisation, the ProgramData/AppData/USERPROFILE trio
	// for anything that resolves a user or machine path, and PATHEXT for command
	// lookup.
	"SystemRoot", "SystemDrive", "windir", "COMSPEC", "PATHEXT",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData",
	"ProgramFiles", "ProgramFiles(x86)", "CommonProgramFiles",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",

	// Temporary directories: servers write scratch files, and a missing TMPDIR
	// sends some runtimes to a path they cannot create.
	"TMPDIR", "TEMP", "TMP",
}

// BaseEnv returns the environment a server process starts with: the allow-listed
// variables that are actually set, plus each requested name from extra.
//
// extra holds the values of the server's envFrom entries (its own tokens). They
// are passed as name=value pairs by the caller, which reads them from the user's
// environment — mcp.json itself never contains a secret.
func BaseEnv(extra map[string]string) []string {
	out := make([]string, 0, len(baseEnvNames)+len(extra))
	seen := map[string]bool{}
	for _, name := range baseEnvNames {
		v, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		out = append(out, name+"="+v)
		seen[envKey(name)] = true
	}
	// Sorted so a server's environment is reproducible between runs, which makes
	// a failing MCP server diagnosable.
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if seen[envKey(name)] {
			// An envFrom entry naming an allow-listed variable would otherwise
			// appear twice, and which one wins is platform-dependent.
			out = replaceEnv(out, name, extra[name])
			continue
		}
		out = append(out, name+"="+extra[name])
		seen[envKey(name)] = true
	}
	return out
}

// envKey normalises a variable name for comparison. Windows environment
// variables are case-insensitive, so "Path" and "PATH" are one variable there
// and two everywhere else.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// replaceEnv overwrites the value of name in env.
func replaceEnv(env []string, name, value string) []string {
	key := envKey(name)
	for i, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 && envKey(kv[:eq]) == key {
			env[i] = name + "=" + value
			return env
		}
	}
	return append(env, name+"="+value)
}
