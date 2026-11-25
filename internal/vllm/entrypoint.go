package vllm

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// Program names recorded in Entrypoint.Program.
const (
	programVLLMServe = "vllm serve"
	programAPIServer = "python -m vllm.entrypoints.openai.api_server"
)

// imageEntrypoint records the default entrypoint of a known vLLM image.
//
// The table is pinned to names.SupportedVLLMVersion: each entry was read from
// the vLLM v0.33.0 build files (docker/Dockerfile, docker/Dockerfile.cpu,
// docker/Dockerfile.rocm all end with ENTRYPOINT ["vllm", "serve"]). An image
// whose entrypoint is not in this table cannot be resolved without executing it,
// so it is rejected.
type imageEntrypoint struct {
	// Repository is the image repository path without registry host, tag, or
	// digest. Any registry host is accepted because registries mirror the same
	// image content; the repository path is what identifies the entrypoint.
	Repository string
	// Program is the server program the image starts.
	Program string
}

// imageEntrypoints is the versioned table of known image entrypoints.
var imageEntrypoints = []imageEntrypoint{
	{Repository: "vllm/vllm-openai", Program: programVLLMServe},
	{Repository: "vllm/vllm-openai-cpu", Program: programVLLMServe},
	{Repository: "vllm/vllm-openai-rocm", Program: programVLLMServe},
}

// wrapperEntrypointTagSuffixes are published tag variants whose default
// entrypoint is a wrapper script rather than the vLLM server (the SageMaker and
// non-root build targets of the same repository). They cannot be resolved
// statically even though the repository is known.
var wrapperEntrypointTagSuffixes = []string{"sagemaker", "nonroot", "non-root"}

// pythonModuleEntrypoints are the Python module invocation forms that start the
// OpenAI-compatible server. The value is the program name and whether the module
// requires the trailing "serve" subcommand.
var pythonModuleEntrypoints = map[string]struct {
	program      string
	needsServe   bool
	acceptsServe bool
}{
	"vllm.entrypoints.openai.api_server": {program: programAPIServer},
	"vllm.entrypoints.cli.main":          {program: "python -m vllm.entrypoints.cli.main serve", needsServe: true, acceptsServe: true},
}

// pythonInterpreter matches the interpreter names vLLM images use.
var pythonInterpreter = regexp.MustCompile(`^python(\d+(\.\d+)?)?$`)

// vllmSubcommands are the subcommands of the vllm CLI in release 0.33.0. Only
// "serve" starts the OpenAI-compatible server; the others are record/benchmark
// tooling and change the meaning of the same arguments.
var vllmSubcommands = map[string]bool{
	"serve":            true,
	"bench":            true,
	"chat":             true,
	"complete":         true,
	"launch":           true,
	"collect-env":      true,
	"run-batch":        true,
	"download-kernels": true,
	"snapshot":         true,
	"preload":          true,
}

// shellInterpreters are shells; a "sh -c" style entrypoint hides the real
// invocation inside a string, which this package refuses to interpret.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true,
	"ash": true, "busybox": true, "fish": true, "csh": true, "tcsh": true,
}

// detectEntrypoint resolves how the container starts vLLM and returns the
// arguments that configure the server.
func detectEntrypoint(inv Invocation) (Entrypoint, []string, *Rejection) {
	command := inv.Container.Command

	if len(command) == 0 {
		return imageDefaultEntrypoint(inv.Container)
	}

	all := append(append([]string{}, command...), inv.Container.Args...)
	base := path.Base(command[0])
	lower := strings.ToLower(base)

	if shellInterpreters[lower] {
		return Entrypoint{}, nil, &Rejection{
			Setting: "command",
			Value:   strings.Join(command, " "),
			Reason:  "shell wrapper entrypoints hide the vLLM invocation in a program string; run vllm directly instead",
		}
	}

	if pythonInterpreter.MatchString(lower) {
		return pythonModuleEntrypoint(base, all)
	}

	if lower == "vllm" {
		return vllmCLIEntrypoint(all)
	}

	reason := fmt.Sprintf("%q is not a supported vLLM entrypoint", command[0])
	if strings.HasSuffix(lower, ".sh") || strings.HasSuffix(lower, ".py") || strings.HasSuffix(lower, ".bash") {
		reason = "wrapper script entrypoints are not a supported vLLM entrypoint; the vLLM invocation inside them is not resolvable without executing them"
	}
	return Entrypoint{}, nil, &Rejection{
		Setting: "command",
		Value:   strings.Join(command, " "),
		Reason:  reason,
	}
}

// pythonModuleEntrypoint resolves "python -m vllm.entrypoints.<module> [serve]".
func pythonModuleEntrypoint(interpreter string, all []string) (Entrypoint, []string, *Rejection) {
	// all = [interpreter, "-m", module, ...rest]
	if len(all) < 3 || all[1] != "-m" {
		return Entrypoint{}, nil, &Rejection{
			Setting: "command",
			Value:   strings.Join(all, " "),
			Reason:  "a Python entrypoint must run a vLLM server module with `-m`; other Python invocations cannot be resolved",
		}
	}
	module := all[2]
	entry, ok := pythonModuleEntrypoints[module]
	if !ok {
		return Entrypoint{}, nil, &Rejection{
			Setting: "command",
			Value:   module,
			Reason:  "unsupported Python module; supported server modules are vllm.entrypoints.openai.api_server and vllm.entrypoints.cli.main",
		}
	}
	rest := all[3:]
	program := entry.program
	if entry.needsServe {
		if len(rest) == 0 || rest[0] != "serve" {
			return Entrypoint{}, nil, &Rejection{
				Setting: "command",
				Value:   strings.Join(all, " "),
				Reason:  "the vllm.entrypoints.cli.main module requires the \"serve\" subcommand",
			}
		}
		rest = rest[1:]
	}
	program = strings.Replace(program, "python", interpreter, 1)
	return Entrypoint{Kind: EntrypointPythonModule, Program: program}, rest, nil
}

// vllmCLIEntrypoint resolves "vllm serve ...".
func vllmCLIEntrypoint(all []string) (Entrypoint, []string, *Rejection) {
	if len(all) < 2 {
		return Entrypoint{}, nil, &Rejection{
			Setting: "command",
			Value:   "vllm",
			Reason:  "the vllm CLI requires a subcommand; use \"vllm serve\"",
		}
	}
	sub := all[1]
	if sub != "serve" {
		reason := fmt.Sprintf("the vllm subcommand %q does not start the OpenAI-compatible server; use \"vllm serve\"", sub)
		if !vllmSubcommands[sub] {
			reason = fmt.Sprintf("%q is not a vllm subcommand; use \"vllm serve\"", sub)
		}
		return Entrypoint{}, nil, &Rejection{Setting: "command", Value: "vllm " + sub, Reason: reason}
	}
	return Entrypoint{Kind: EntrypointCommand, Program: programVLLMServe}, all[2:], nil
}

// imageDefaultEntrypoint resolves the entrypoint of a known image when the
// container does not override the command.
func imageDefaultEntrypoint(c corev1.Container) (Entrypoint, []string, *Rejection) {
	repository, tag := splitImageReference(c.Image)
	for _, suffix := range wrapperEntrypointTagSuffixes {
		if strings.HasSuffix(tag, suffix) {
			return Entrypoint{}, nil, &Rejection{
				Setting: "image",
				Value:   c.Image,
				Reason:  "this image tag publishes a wrapper-script default entrypoint, which cannot be resolved without executing it",
			}
		}
	}
	for _, entry := range imageEntrypoints {
		if entry.Repository != repository {
			continue
		}
		if len(c.Args) > 0 && repeatsCommand(c.Args[0]) {
			return Entrypoint{}, nil, &Rejection{
				Setting: "command",
				Value:   c.Args[0],
				Reason:  "the image entrypoint already runs the vLLM server, so the container arguments must not repeat the command; they start with the model",
			}
		}
		return Entrypoint{
			Kind:         EntrypointImageDefault,
			Program:      entry.Program,
			Image:        c.Image,
			TableVersion: names.SupportedVLLMVersion,
		}, c.Args, nil
	}
	return Entrypoint{}, nil, &Rejection{
		Setting: "image",
		Value:   c.Image,
		Reason:  "no default entrypoint is recorded for this image, so the vLLM invocation cannot be resolved; set the container command explicitly",
	}
}

// repeatsCommand reports whether an image-default argument looks like a command
// rather than a model or flag. Kubernetes appends container args to the image
// entrypoint, so a command here would be executed as the model name.
func repeatsCommand(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") {
		return false
	}
	base := strings.ToLower(path.Base(arg))
	return vllmSubcommands[base] ||
		base == "vllm" ||
		pythonInterpreter.MatchString(base) ||
		shellInterpreters[base]
}

// splitImageReference returns the repository path and tag of an image
// reference, ignoring registry host, digest and tag punctuation.
func splitImageReference(image string) (repository, tag string) {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	// Drop a registry host, which may contain dots or a port.
	if i := strings.Index(ref, "/"); i >= 0 {
		head := ref[:i]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			ref = ref[i+1:]
		}
	}
	ref = strings.TrimPrefix(ref, "library/")
	return ref, tag
}
