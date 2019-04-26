package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.coder.com/retry"
	"golang.org/x/crypto/ssh"
)

func TestSSHCode(t *testing.T) {
	// Avoid opening a browser window.
	err := os.Unsetenv("DISPLAY")
	require.NoError(t, err)

	// start up our jank ssh server
	trassh(t, "10010")

	const (
		localPort  = "9090"
		remotePort = "9091"
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := sshCode("127.0.0.1", "", options{
			sshFlags:   "-p 10010",
			localPort:  localPort,
			remotePort: remotePort,
		})
		require.NoError(t, err)
	}()

	waitForSSHCode(t, localPort, time.Second*30)
	waitForSSHCode(t, remotePort, time.Second*30)

	out, err := exec.Command("pkill", "codessh").CombinedOutput()
	require.NoError(t, err, "%s", out)

	wg.Wait()
}

// trassh is a incomplete, local, insecure ssh server
// used for the purpose of testing the implementation without
// requiring the user to use have their own remote server.
func trassh(t *testing.T, port string) {
	private, err := ssh.ParsePrivateKey([]byte(fakeRSAKey))
	require.NoError(t, err)

	conf := &ssh.ServerConfig{
		NoClientAuth: true,
	}

	conf.AddHostKey(private)

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	require.NoError(t, err)

	go func() {
		for {
			func() {
				conn, err := listener.Accept()
				require.NoError(t, err)
				defer conn.Close()

				sshConn, chans, reqs, err := ssh.NewServerConn(conn, conf)
				require.NoError(t, err)

				go ssh.DiscardRequests(reqs)

				for c := range chans {
					switch c.ChannelType() {
					case "direct-tcpip":
						var req directTCPIPReq

						err := ssh.Unmarshal(c.ExtraData(), &req)
						if err != nil {
							t.Logf("failed to unmarshal tcpip data: %v", err)
							continue
						}

						ch, _, err := c.Accept()
						if err != nil {
							c.Reject(ssh.ConnectionFailed, fmt.Sprintf("unable to accept channel: %v", err))
							continue
						}

						go handleDirectTCPIP(ch, &req, t)
					case "session":
						ch, inReqs, err := c.Accept()
						if err != nil {
							c.Reject(ssh.ConnectionFailed, fmt.Sprintf("unable to accept channel: %v", err))
							continue
						}

						go handleSession(ch, inReqs, t)
					default:
						fmt.Printf("unsupported session type: %v\n", c.ChannelType())
						c.Reject(ssh.UnknownChannelType, "unknown channel type")
					}
				}

				sshConn.Wait()
			}()
		}
	}()
}

func handleDirectTCPIP(ch ssh.Channel, req *directTCPIPReq, t *testing.T) {
	defer ch.Close()

	dstAddr := net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))

	conn, err := net.Dial("tcp", dstAddr)
	if err != nil {
		return
	}
	defer conn.Close()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ch.Close()

		io.Copy(ch, conn)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer conn.Close()

		io.Copy(conn, ch)
	}()
	wg.Wait()
}

// execReq describes an exec payload.
type execReq struct {
	Command string
}

// directTCPIPReq describes the extra data sent in a
// direct-tcpip request containing the host/port for the ssh server.
type directTCPIPReq struct {
	Host string
	Port uint32

	Orig     string
	OrigPort uint32
}

// exitStatus describes an 'exit-status' message
// returned after a request.
type exitStatus struct {
	Status uint32
}

func handleSession(ch ssh.Channel, in <-chan *ssh.Request, t *testing.T) {
	defer ch.Close()

	for req := range in {
		if req.WantReply {
			req.Reply(true, nil)
		}

		// TODO support the rest of the types e.g. env, pty, etc.
		// Right now they aren't necessary for the tests.
		if req.Type != "exec" {
			t.Logf("Unsupported session type %v, only 'exec' is supported", req.Type)
			continue
		}

		var exReq execReq
		err := ssh.Unmarshal(req.Payload, &exReq)
		if err != nil {
			t.Logf("failed to unmarshal exec payload %s", req.Payload)
			return
		}

		cmd := exec.Command("sh", "-c", exReq.Command)

		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)

		go func() {
			defer stdin.Close()
			io.Copy(stdin, ch)
		}()

		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		err = cmd.Run()

		var exit exitStatus
		if err != nil {
			t.Logf("exec err: %v", err)
			exErr, ok := err.(*exec.ExitError)
			require.True(t, ok, "Not an exec.ExitError, was %T", err)

			exit.Status = uint32(exErr.ExitCode())
		}

		_, err = ch.SendRequest("exit-status", false, ssh.Marshal(&exit))
		if err != nil {
			t.Logf("unable to send status: %v", err)
		}
		break
	}
}

func waitForSSHCode(t *testing.T, port string, timeout time.Duration) {
	var (
		url    = fmt.Sprintf("http://localhost:%v/", port)
		client = &http.Client{
			Timeout: time.Second,
		}
	)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	backoff := &retry.Backoff{
		Floor: time.Second,
		Ceil:  time.Second,
	}

	for {
		resp, err := client.Get(url)
		if err == nil {
			require.Equal(t, http.StatusOK, resp.StatusCode)
			return
		}
		err = backoff.Wait(ctx)
		require.NoError(t, err)
	}
}

const fakeRSAKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEpQIBAAKCAQEAsbbGAxPQeqti2OgdzuMgJGBAwXe/bFhQTPuk0bIvavkZwX/a
NhmXV0dhLino5KtjR8oEazLxOgnOkJ6mpwVEgUhNMZhD9jEHZ7at4DtBIwfxjHjv
nF+kJAt4xX4AZYbwIfLN9TsDGGhv4wPlB7mbwv+lhmPK+HsLbajO4n69k3s0WW94
LafJntx/98o9gL2R7hpbMxgUu8cSZjYakkRBQdab0xUuTiceq0HfAOBCQpEw0meF
cmhMeeu7H5UwKGj573pBxON0G1SJgipkcs4TD2rZ9wjc29gDJjHjf3Ko/JzX1WFL
db21fzqRGWelgCHCUsIvUBeExk4jM1d63JrmFQIDAQABAoIBAQCdc9OSjG6tEMYe
aeFnGQK0V/dnskIOq1xSKK7J/7ZVb+iq8S0Tu67D7IEklos6dsMaqtkpZVQm2OOE
bJw45MjiRn3mUAL+0EfAUzFQtw8qC3Kuw8N/55kVOnjBeba+PUTqvyZNfQBsErP3
Dc9Q/dkMdtZf8HC3oMTqXqMWN7adQBQRBspUBkLQeSemYsUm2cc+YSnCwKel98uN
EuDJaTZwutxTUF1FBoXlejYlVKcldk1w5HtKkjGdW+mbo2xUpu8W0620Rs/fXNpU
+guAlpB1/Wx5foZqZx33Ul8HINfDre/uqHwCd+ucDIyV7TfIh9JV5w3iRLa0QCz0
kFe/GsEtAoGBAODRa1GwfyK+gcgxF2qwfsxF3I+DQhqWFiCA0o5kO2fpiUR3rDQj
XhBoPxr/qYBSBtGErHIiB7WFeQ6GjVTEgY/cEkIIh1tY95UWQ3/oIZWW498dQGRh
SUGXm3lMrSsVCyXxNexSH5yTrRzyZ2u4mZupMeyACoGRGkNTVppOU4XbAoGBAMpc
1ifX3kr5m8CXa6mI+NWFAQlhW0Ak0hjhM/WDzMrSimYxLLSkaKyUSHnFP/8V4asA
tV173lVut2Cjv5v5FcrOnI33Li2IcNlOzCRiLHzZ43HXckcoQDcU8iKTBq1a0Dx1
eXr2rs+a/2pTy7IMsxyJVCSP6IDBI9+2iW+Cxh7PAoGBAMOa0hJAS02yjX7d367v
I1OeETo4jQJOxa/ABfLoGJvfoJQWv5iZkRUbbpSSDytbsx0Gn3eqTiTMnbhar4sq
ckP1yVj0zLhY3wkzVsVp9haOM3ODouvzjWZpf1d5tE2AwLNhfHZCOcjk4EEIU51w
/w1ll89a1ElJM52SXA5jyd3zAoGBAKGtpKi2rvMGFKu+DxWnyu+FUXu2HhrUkEuy
ejn5MMEHj+3v8gDtrnfcDT/FGclrKR7f9QeYtN1bFQYQLkGmtAOSKcC/MVTNwyPL
8gxLp7GkwDSvZq11ekDH6mE3SMluWhtD3Ggi+S4Db3f7NS6vONde3SxNEfz00v2l
MI84U6Q/AoGAVTZGT5weqRTJSqnri6Noz+5j/73QMf/QiZDgHMMCF0giC2mxqOgR
QF6+cxHQe0sbMQ/xJU5RYhgnqSa2TjLMju4N2nQ9i/HqI/3p0CPwjFsZWlXmWEK9
5kdld52W7Bu2vQuFbg2Oy7aPhnI+1CqlubOFRgMe4AJND2t9SMTV+rc=
-----END RSA PRIVATE KEY-----
`
