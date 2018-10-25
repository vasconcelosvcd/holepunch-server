package sshserverportforward

import (
	"errors"
	"fmt"
	"github.com/function61/gokit/bidipipe"
	"github.com/function61/gokit/logger"
	"golang.org/x/crypto/ssh"
	"net"
	"strconv"
	"time"
)

var log = logger.New("sshd-portforward")

var (
	errUnsupportedAddress = errors.New("unsupported address")
)

// returns a new channel that receives all non-portforwarding requests.
// if you don't do anything with them call "go ssh.DiscardRequests()"
func ProcessPortForwardRequests(requests <-chan *ssh.Request, serverConn *ssh.ServerConn) <-chan *ssh.Request {
	nonForwardRequests := make(chan *ssh.Request, 1)

	go func() {
		for req := range requests {
			if req.Type != "tcpip-forward" && req.Type != "cancel-tcpip-forward" {
				nonForwardRequests <- req
				continue
			}

			// does not block for a long time
			processOnePortForwardRequest(req, serverConn)
		}
	}()

	return nonForwardRequests
}

func processOnePortForwardRequest(req *ssh.Request, serverConn *ssh.ServerConn) {
	if req.Type != "tcpip-forward" {
		panic("cancel-tcpip-forward not yet implemented")
	}

	var cfm channelForwardMsg
	if err := ssh.Unmarshal(req.Payload, &cfm); err != nil {
		log.Error(err.Error())
		req.Reply(false, nil)
		return
	}

	if cfm.Addr != "127.0.0.1" && cfm.Addr != "0.0.0.0" {
		// we don't support non-local addresses yet (Dial()ing)
		log.Error(errUnsupportedAddress.Error())
		req.Reply(false, nil)
		return
	}

	listenAddr := fmt.Sprintf("%s:%d", cfm.Addr, cfm.Rport)

	log.Info(fmt.Sprintf("Adding listener to %s", listenAddr))

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Error(err.Error())
		req.Reply(false, nil)
		return
	}

	go func() {
		for {
			connToForward, err := listener.Accept()
			if err != nil {
				log.Error(fmt.Sprintf("Accept() failed: %s", err.Error()))
				time.Sleep(500 * time.Millisecond) // prevent hot loop
				continue
			}

			log.Debug(fmt.Sprintf("new client: %s", connToForward.RemoteAddr().String()))

			go func() {
				if err := forwardOneReverseConnection(serverConn, connToForward, cfm); err != nil {
					log.Error(fmt.Sprintf("forwardOneReverseConnection(): %s", err.Error()))
				}
			}()
		}
	}()

	/*	FIXME: we probably should implement to-spec where responding with port if port in req was 0

		type channelForwardResponse struct {
			Port uint32
		}
	*/
	req.Reply(true, nil)
}

func forwardOneReverseConnection(sshServerConn *ssh.ServerConn, connToForward net.Conn, cfm channelForwardMsg) error {
	remoteAddr := connToForward.RemoteAddr()
	remoteHost, remotePortStr, err := net.SplitHostPort(remoteAddr.String())
	if err != nil {
		return err
	}

	remotePort, err := strconv.Atoi(remotePortStr)
	if err != nil {
		return err
	}

	fordwardedMsg := &forwardedTCPPayload{
		Addr:       cfm.Addr,
		Port:       cfm.Rport,
		OriginAddr: remoteHost,
		OriginPort: uint32(remotePort),
	}

	ch, reqs, err := sshServerConn.OpenChannel("forwarded-tcpip", ssh.Marshal(fordwardedMsg))
	if err != nil {
		return err
	}

	// we're not expecting any requests for this channel
	go ssh.DiscardRequests(reqs)

	return bidipipe.Pipe(ch, "SSH tunnel", connToForward, "Local connection")
}