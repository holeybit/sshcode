package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pkg/browser"
	"go.coder.com/flog"
	"golang.org/x/xerrors"
)

func init() {
	rand.Seed(time.Now().Unix())
}

const helpTabWidth = 5

var helpTab = strings.Repeat(" ", helpTabWidth)

// flagHelp generates a friendly help string for all globally registered command
// line flags.
func flagHelp() string {
	var bd strings.Builder

	w := tabwriter.NewWriter(&bd, 3, 10, helpTabWidth, ' ', 0)

	fmt.Fprintf(w, "Flags:\n")
	var count int
	flag.VisitAll(func(f *flag.Flag) {
		count++
		if f.DefValue == "" {
			fmt.Fprintf(w, "\t-%v\t%v\n", f.Name, f.Usage)
		} else {
			fmt.Fprintf(w, "\t-%v\t%v\t(%v)\n", f.Name, f.Usage, f.DefValue)
		}
	})
	if count == 0 {
		return "\n"
	}

	w.Flush()

	return bd.String()
}

// version is overwritten by ci/build.sh.
var version string

func main() {
	var (
		skipSyncFlag = flag.Bool("skipsync", false, "skip syncing local settings and extensions to remote host")
		sshFlags     = flag.String("ssh-flags", "", "custom SSH flags")
		syncBack     = flag.Bool("b", false, "sync extensions back on termination")
		printVersion = flag.Bool("version", false, "print version information and exit")
		remotePort   = flag.String("remote-port", "", "Start VS Code on the provided port. If one is not provided a random one is selected")
		localPort    = flag.String("local-port", "", "Local port used to connect to VS Code. If one is not provided a random one is selected")
	)

	flag.Usage = func() {
		fmt.Printf(`Usage: %v [FLAGS] HOST [DIR]
Start VS Code via code-server over SSH.

Environment variables:
		`+vsCodeConfigDirEnv+`	use special VS Code settings dir.
		`+vsCodeExtensionsDirEnv+`	use special VS Code extensions dir.

More info: https://github.com/cdr/sshcode

Arguments:
`+helpTab+`HOST is passed into the ssh command.
`+helpTab+`DIR is optional.

%v`, os.Args[0], flagHelp(),
		)
	}

	flag.Parse()
	if *printVersion {
		fmt.Printf("%v\n", version)
		os.Exit(0)
	}

	host := flag.Arg(0)

	if host == "" {
		// If no host is specified output the usage.
		flag.Usage()
		os.Exit(1)
	}

	dir := flag.Arg(1)
	if dir == "" {
		dir = "~"
	}

	err := sshCode(host, dir, options{
		skipSync:   *skipSyncFlag,
		sshFlags:   *sshFlags,
		syncBack:   *syncBack,
		localPort:  *localPort,
		remotePort: *remotePort,
	})

	if err != nil {
		flog.Fatal("error: %v", err)
	}
}

type options struct {
	skipSync   bool
	syncBack   bool
	localPort  string
	remotePort string
	sshFlags   string
}

func sshCode(host, dir string, o options) error {
	flog.Info("ensuring code-server is updated...")

	const codeServerPath = "/tmp/codessh-code-server"

	dlScript := downloadScript(codeServerPath)

	// Downloads the latest code-server and allows it to be executed.
	sshCmdStr := fmt.Sprintf("ssh" +
		" " + o.sshFlags + " " +
		host + " /bin/bash",
	)

	fmt.Printf("ssh: %v\n", sshCmdStr)

	sshCmd := exec.Command("sh", "-c", sshCmdStr)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	sshCmd.Stdin = strings.NewReader(dlScript)
	err := sshCmd.Run()
	if err != nil {
		return xerrors.Errorf("failed to update code-server: \n---ssh cmd---\n%s\n---download script---\n%s: %w",
			sshCmdStr,
			dlScript,
			err,
		)
	}

	if !o.skipSync {
		start := time.Now()
		flog.Info("syncing settings")
		err = syncUserSettings(o.sshFlags, host, false)
		if err != nil {
			return xerrors.Errorf("failed to sync settings: %w", err)
		}

		flog.Info("synced settings in %s", time.Since(start))

		flog.Info("syncing extensions")
		err = syncExtensions(o.sshFlags, host, false)
		if err != nil {
			return xerrors.Errorf("failed to sync extensions: %w", err)
		}
		flog.Info("synced extensions in %s", time.Since(start))
	}

	flog.Info("starting code-server...")

	if o.localPort == "" {
		o.localPort, err = randomPort()
	}
	if err != nil {
		return xerrors.Errorf("failed to find available local port: %w", err)
	}

	// TODO pick a random remote port
	if o.remotePort == "" {
		o.remotePort, err = randomRemotePort(host, o.sshFlags)
	}
	if err != nil {
		return xerrors.Errorf("failed to find available remote port: %w", err)
	}

	flog.Info("Tunneling local port %v to remote port %v", o.localPort, o.remotePort)

	sshCmdStr = fmt.Sprintf("ssh -tt -q -L %v %v %v 'cd %v; %v --host 127.0.0.1 --allow-http --no-auth --port=%v'",
		o.localPort+":localhost:"+o.remotePort, o.sshFlags, host, dir, codeServerPath, o.remotePort,
	)

	// Starts code-server and forwards the remote port.
	sshCmd = exec.Command("sh", "-c", sshCmdStr)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	err = sshCmd.Start()
	if err != nil {
		flog.Fatal("failed to start code-server: %v", err)
	}

	url := "http://127.0.0.1:" + o.localPort
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := http.Client{
		Timeout: time.Second * 3,
	}
	for {
		if ctx.Err() != nil {
			return xerrors.Errorf("code-server didn't start in time: %w", ctx.Err())
		}
		// Waits for code-server to be available before opening the browser.
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		resp.Body.Close()
		break
	}

	ctx, cancel = context.WithCancel(context.Background())

	if os.Getenv("DISPLAY") != "" {
		openBrowser(url)
	}

	go func() {
		defer cancel()
		sshCmd.Wait()
	}()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)

	select {
	case <-ctx.Done():
	case <-c:
	}

	if !o.syncBack || o.skipSync {
		flog.Info("shutting down")
		return nil
	}

	flog.Info("synchronizing VS Code back to local")

	err = syncExtensions(o.sshFlags, host, true)
	if err != nil {
		return xerrors.Errorf("failed to sync extensions back: %w", err)
	}

	err = syncUserSettings(o.sshFlags, host, true)
	if err != nil {
		return xerrors.Errorf("failed to sync user settings settings back: %w", err)
	}

	return nil
}

func openBrowser(url string) {
	var openCmd *exec.Cmd

	const (
		macPath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		wslPath = "/mnt/c/Program Files (x86)/Google/Chrome/Application/chrome.exe"
	)

	switch {
	case commandExists("google-chrome"):
		openCmd = exec.Command("google-chrome", chromeOptions(url)...)
	case commandExists("google-chrome-stable"):
		openCmd = exec.Command("google-chrome-stable", chromeOptions(url)...)
	case commandExists("chromium"):
		openCmd = exec.Command("chromium", chromeOptions(url)...)
	case commandExists("chromium-browser"):
		openCmd = exec.Command("chromium-browser", chromeOptions(url)...)
	case pathExists(macPath):
		openCmd = exec.Command(macPath, chromeOptions(url)...)
	case pathExists(wslPath):
		openCmd = exec.Command(wslPath, chromeOptions(url)...)
	default:
		err := browser.OpenURL(url)
		if err != nil {
			flog.Error("failed to open browser: %v", err)
		}
		return
	}

	// We do not use CombinedOutput because if there is no chrome instance, this will block
	// and become the parent process instead of using an existing chrome instance.
	err := openCmd.Start()
	if err != nil {
		flog.Error("failed to open browser: %v", err)
	}
}

func chromeOptions(url string) []string {
	return []string{"--app=" + url, "--disable-extensions", "--disable-plugins", "--incognito"}
}

// Checks if a command exists locally.
func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func pathExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// randomPort picks a random port to start code-server on.
func randomPort() (string, error) {
	const (
		minPort  = 1024
		maxPort  = 65535
		maxTries = 10
	)
	for i := 0; i < maxTries; i++ {
		port := rand.Intn(maxPort-minPort+1) + minPort
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			_ = l.Close()
			return strconv.Itoa(port), nil
		}
		flog.Info("port taken: %d", port)
	}

	return "", xerrors.Errorf("max number of tries exceeded: %d", maxTries)
}

func syncUserSettings(sshFlags string, host string, back bool) error {
	localConfDir, err := configDir()
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	var remoteSettingsDir = filepath.Join(home, ".local/share/code-server/User/")

	var (
		src  = localConfDir + "/"
		dest = host + ":" + remoteSettingsDir
	)

	if back {
		dest, src = src, dest
	}

	// Append "/" to have rsync copy the contents of the dir.
	return rsync(src, dest, sshFlags, "workspaceStorage", "logs", "CachedData")
}

func syncExtensions(sshFlags string, host string, back bool) error {
	localExtensionsDir, err := extensionsDir()
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	var remoteExtensionsDir = filepath.Join(home, ".local/share/code-server/extensions/")

	var (
		src  = localExtensionsDir + "/"
		dest = host + ":" + remoteExtensionsDir
	)
	if back {
		dest, src = src, dest
	}

	return rsync(src, dest, sshFlags)
}

func rsync(src string, dest string, sshFlags string, excludePaths ...string) error {
	excludeFlags := make([]string, len(excludePaths))
	for i, path := range excludePaths {
		excludeFlags[i] = "--exclude=" + path
	}

	cmd := exec.Command("rsync", append(excludeFlags, "-azvr",
		"-e", "ssh "+sshFlags,
		// Only update newer directories, and sync times
		// to keep things simple.
		"-u", "--times",
		// This is more unsafe, but it's obnoxious having to enter VS Code
		// locally in order to properly delete an extension.
		"--delete",
		"--copy-unsafe-links",
		src, dest,
	)...,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return xerrors.Errorf("failed to rsync '%s' to '%s': %w", src, dest, err)
	}

	return nil
}

func downloadScript(codeServerPath string) string {
	return fmt.Sprintf(
		`set -euxo pipefail || exit 1

mkdir -p ~/.local/share/code-server
cd %v
wget -N https://codesrv-ci.cdr.sh/latest-linux
[ -f %v ] && rm %v
ln latest-linux %v
chmod +x %v`,
		filepath.Dir(codeServerPath),
		codeServerPath,
		codeServerPath,
		codeServerPath,
		codeServerPath,
	)

}

func randomRemotePort(host, sshFlags string) (string, error) {
	var (
		sshCmdStr = fmt.Sprintf("ssh %v %v /bin/bash", sshFlags, host)
		buf       bytes.Buffer
	)

	cmd := exec.Command("sh", "-c", sshCmdStr)
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	cmd.Stdin = strings.NewReader(randomRemotePortScript)
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(buf.String()), nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

const randomRemotePortScript = `
read LOWERPORT UPPERPORT < /proc/sys/net/ipv4/ip_local_port_range
while :
do
        PORT="$(shuf -i $LOWERPORT-$UPPERPORT -n 1)"
        ss -lpn | grep -q ":$PORT " || break
done
echo $PORT`
