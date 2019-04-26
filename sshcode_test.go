package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestSSHCode(t *testing.T) {
	err := os.Unsetenv("DISPLAY")
	require.NoError(t, err)

	sshServer(t, "10010")

	const (
		localPort  = "9090"
		remotePort = "9091"
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sshCode("127.0.0.1", "", options{
			sshFlags:   "-p 10010",
			localPort:  localPort,
			remotePort: remotePort,
		})
	}()

	waitForSSHCode(t, localPort, time.Second*5)
	waitForSSHCode(t, remotePort, time.Second*5)
	fmt.Println("Fuck yeah")
	wg.Wait()
}

func sshClient(t *testing.T, sshPort string) {
	conf := &ssh.ClientConfig{
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", sshPort))
	require.NoError(t, err)

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "", conf)
	require.NoError(t, err)

	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	require.NoError(t, err)
	defer sess.Close()

	out, err := sess.CombinedOutput("cat /home/jon/test.txt")
	if err != nil && err != io.EOF {
		fmt.Printf("combined output err: %v\n", err)
	}

	fmt.Printf("out: %s\n", out)
}

func keyAuth(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	fmt.Printf("authenticate with %v\n", key.Type())
	return nil, nil
}

func sshServer(t *testing.T, port string) {
	// replace with a bogus key
	privKey, err := ioutil.ReadFile("/home/jon/.ssh/id_rsa")
	require.NoError(t, err)

	private, err := ssh.ParsePrivateKey(privKey)
	require.NoError(t, err)

	conf := &ssh.ServerConfig{
		PublicKeyCallback: keyAuth,
		NoClientAuth:      true,
	}

	conf.AddHostKey(private)

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	require.NoError(t, err)

	addr := listener.Addr().String()
	fmt.Printf("listening on: %v\n", addr)

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
						var req DirectTCPIPReq

						err := ssh.Unmarshal(c.ExtraData(), &req)
						if err != nil {
							fmt.Printf("direct-tcpip marshal err: %v\n", err)
							continue
						}

						ch, _, err := c.Accept()
						if err != nil {
							fmt.Printf("accept err: %v\n", err)
						}

						go handleDirectTCPIP(ch, &req, t)
					case "session":
						ch, inReqs, err := c.Accept()
						require.NoError(t, err)

						go func() {
							handler(ch, inReqs, t)
						}()
					default:
						fmt.Printf("UNKNOWN SESSION TYPE: %v\n", c.ChannelType())
						c.Reject(ssh.UnknownChannelType, "unknown channel type")
					}
				}

				err = sshConn.Wait()
				fmt.Printf("server exit reason: %v\n", err)
			}()
		}
	}()
}

func handleDirectTCPIP(ch ssh.Channel, req *DirectTCPIPReq, t *testing.T) {
	dstAddr := net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))

	conn, err := net.Dial("tcp", dstAddr)
	if err != nil {
		fmt.Printf("dial err: %v\n", err)
		return
	}
	defer conn.Close()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(ch, conn)
		ch.Close()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(conn, ch)
		conn.Close()
	}()
	wg.Wait()
}

type ExecReq struct {
	Command string
}

type DirectTCPIPReq struct {
	Host string
	Port uint32

	Orig     string
	OrigPort uint32
}

func handler(ch ssh.Channel, in <-chan *ssh.Request, t *testing.T) {
	defer ch.Close()
	ch.Read(nil)

	for req := range in {
		// TODO support the rest of the types e.g. env, pty, etc.
		// Right now they aren't necessary for the tests.
		if req.Type != "exec" {
			t.Logf("Unsupported session type %v, only 'exec' is supported", req.Type)
			if req.WantReply {
				req.Reply(true, nil)
			}
			continue
		}
		req.Reply(true, nil)

		var exReq ExecReq
		err := ssh.Unmarshal(req.Payload, &exReq)
		fmt.Printf("command: %s\n", exReq.Command)

		cmd := exec.Command("sh", "-c", exReq.Command)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			fmt.Printf("stdin err: %v\n", err)
		}

		go func() {
			io.Copy(stdin, ch)
			stdin.Close()
		}()

		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		err = cmd.Run()
		if err != nil {
			fmt.Printf("start err: %v\n", err)
		}
		var exitCode uint32
		if err != nil {
			fmt.Printf("err: %v\n", err)
			// fmt.Printf("out: %s\n", out)
			// ch.Stderr().Write(out)
			exErr := err.(*exec.ExitError)
			exitCode = uint32(exErr.ExitCode())
		}

		msg := exitStatusMsg{
			Status: exitCode,
		}
		_, err = ch.SendRequest("exit-status", false, ssh.Marshal(&msg))
		if err != nil {
			t.Errorf("unable to send status: %v", err)
		}
		break
	}
}

type exitStatusMsg struct {
	Status uint32
}

func waitForSSHCode(t *testing.T, port string, timeout time.Duration) {
	var (
		timer  = time.NewTimer(timeout)
		url    = fmt.Sprintf("http://localhost:%v/", port)
		client = &http.Client{
			Timeout: time.Second,
		}
	)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			t.Fatal("Timed out waiting for code-server to start")
		default:
			resp, err := client.Get(url)
			if err == nil {
				require.Equal(t, http.StatusOK, resp.StatusCode)
				return
			}
			time.Sleep(time.Second)
		}
	}
}
