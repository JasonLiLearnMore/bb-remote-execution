// +build darwin freebsd linux

package runner

import (
	"context"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/resourceusage"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/runner"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	bb_path "github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/golang/protobuf/ptypes/duration"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// logFileResolver is an implementation of path.ComponentWalker that is
// used by localRunner.Run() to traverse to the directory of stdout and
// stderr log files, so that they may be opened.
//
// TODO: This code seems fairly generic. Should move it to the
// filesystem package?
type logFileResolver struct {
	stack []filesystem.DirectoryCloser
	name  *bb_path.Component
}

func (r *logFileResolver) OnDirectory(name bb_path.Component) (bb_path.GotDirectoryOrSymlink, error) {
	d := r.stack[len(r.stack)-1]
	child, err := d.EnterDirectory(name)
	if err != nil {
		return nil, err
	}
	r.stack = append(r.stack, child)
	return bb_path.GotDirectory{
		Child:        r,
		IsReversible: true,
	}, nil
}

func (r *logFileResolver) OnTerminal(name bb_path.Component) (*bb_path.GotSymlink, error) {
	r.name = &name
	return nil, nil
}

func (r *logFileResolver) OnUp() (bb_path.ComponentWalker, error) {
	if len(r.stack) == 1 {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to a location outside the build directory")
	}
	if err := r.stack[len(r.stack)-1].Close(); err != nil {
		return nil, err
	}
	r.stack = r.stack[:len(r.stack)-1]
	return r, nil
}

func (r *logFileResolver) closeAll() {
	for _, d := range r.stack {
		d.Close()
	}
}

type localRunner struct {
	buildDirectory               filesystem.Directory
	buildDirectoryPath           string
	setTmpdirEnvironmentVariable bool
	chrootIntoInputRoot          bool
}

// NewLocalRunner returns a Runner capable of running commands on the
// local system directly.
func NewLocalRunner(buildDirectory filesystem.Directory, buildDirectoryPath string, setTmpdirEnvironmentVariable, chrootIntoInputRoot bool) Runner {
	return &localRunner{
		buildDirectory:               buildDirectory,
		buildDirectoryPath:           buildDirectoryPath,
		setTmpdirEnvironmentVariable: setTmpdirEnvironmentVariable,
		chrootIntoInputRoot:          chrootIntoInputRoot,
	}
}

func (r *localRunner) openLog(logPath string) (filesystem.FileAppender, error) {
	logFileResolver := logFileResolver{
		stack: []filesystem.DirectoryCloser{filesystem.NopDirectoryCloser(r.buildDirectory)},
	}
	defer logFileResolver.closeAll()
	if err := bb_path.Resolve(logPath, bb_path.NewRelativeScopeWalker(&logFileResolver)); err != nil {
		return nil, err
	}
	if logFileResolver.name == nil {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to a directory")
	}
	d := logFileResolver.stack[len(logFileResolver.stack)-1]
	return d.OpenAppend(*logFileResolver.name, filesystem.CreateExcl(0666))
}

func convertTimeval(t syscall.Timeval) *duration.Duration {
	return &duration.Duration{
		Seconds: int64(t.Sec),
		Nanos:   int32(t.Usec) * 1000,
	}
}

func (r *localRunner) Run(ctx context.Context, request *runner.RunRequest) (*runner.RunResponse, error) {
	if len(request.Arguments) < 1 {
		return nil, status.Error(codes.InvalidArgument, "Insufficient number of command arguments")
	}
	var cmd *exec.Cmd
	if r.chrootIntoInputRoot {
		// The addition of /usr/bin/env is necessary as the PATH resolution
		// will take place prior to the chroot, so the executable may not be
		// found by exec.LookPath() inside exec.CommandContext() and may
		// cause cmd.Start() to fail when it shouldn't.
		// https://github.com/golang/go/issues/39341
		envPrependedArguments := []string{"/usr/bin/env", "--"}
		envPrependedArguments = append(envPrependedArguments, request.Arguments...)
		cmd = exec.CommandContext(ctx, envPrependedArguments[0], envPrependedArguments[1:]...)
		cmd.Dir = filepath.Join("/", request.WorkingDirectory)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Chroot: path.Join(r.buildDirectoryPath, request.InputRootDirectory),
		}
	} else {
		cmd = exec.CommandContext(ctx, request.Arguments[0], request.Arguments[1:]...)
		cmd.Dir = filepath.Join(r.buildDirectoryPath, request.InputRootDirectory, request.WorkingDirectory)
	}
	cmd.Env = make([]string, 0, len(request.EnvironmentVariables)+1)
	if r.setTmpdirEnvironmentVariable && request.TemporaryDirectory != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+filepath.Join(r.buildDirectoryPath, request.TemporaryDirectory))
	}
	for name, value := range request.EnvironmentVariables {
		cmd.Env = append(cmd.Env, name+"="+value)
	}

	// Open output files for logging.
	stdout, err := r.openLog(request.StdoutPath)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to open stdout")
	}
	cmd.Stdout = stdout

	stderr, err := r.openLog(request.StderrPath)
	if err != nil {
		stdout.Close()
		return nil, util.StatusWrap(err, "Failed to open stderr")
	}
	cmd.Stderr = stderr

	// Start the subprocess. We can already close the output files
	// while the process is running.
	err = cmd.Start()
	stdout.Close()
	stderr.Close()
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to start process")
	}

	// Wait for execution to complete. Permit non-zero exit codes.
	if err := cmd.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, err
		}
	}

	// Attach rusage information to the response.
	rusage := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	posixResourceUsage, err := ptypes.MarshalAny(&resourceusage.POSIXResourceUsage{
		UserTime:                   convertTimeval(rusage.Utime),
		SystemTime:                 convertTimeval(rusage.Stime),
		MaximumResidentSetSize:     int64(rusage.Maxrss) * maximumResidentSetSizeUnit,
		PageReclaims:               int64(rusage.Minflt),
		PageFaults:                 int64(rusage.Majflt),
		Swaps:                      int64(rusage.Nswap),
		BlockInputOperations:       int64(rusage.Inblock),
		BlockOutputOperations:      int64(rusage.Oublock),
		MessagesSent:               int64(rusage.Msgsnd),
		MessagesReceived:           int64(rusage.Msgrcv),
		SignalsReceived:            int64(rusage.Nsignals),
		VoluntaryContextSwitches:   int64(rusage.Nvcsw),
		InvoluntaryContextSwitches: int64(rusage.Nivcsw),
	})
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to marshal POSIX resource usage")
	}
	return &runner.RunResponse{
		ExitCode:      int32(cmd.ProcessState.ExitCode()),
		ResourceUsage: []*any.Any{posixResourceUsage},
	}, nil
}
