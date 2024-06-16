package sshd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Channel
type Channel struct {
	channel ssh.Channel

	// out-of-band request
	requests <-chan *ssh.Request
	// environment variables by env requests
	env []string
	// user of this channel
	user *user.User

	// tty for shell
	tty *os.File
	// pty for other end of shell
	pty *os.File

	// baseCtx is the context for this channel,
	// and is used to request the channel to shutdown
	baseCtx context.Context
	// baseCancel cancels the baseCtx
	baseCancel context.CancelFunc

	// sftpServer is the sftp server.
	sftpServer *sftp.Server

	// wg is the wait group used to wait for all the goroutines
	wg *sync.WaitGroup
}

func (c *Channel) Loop() {
reqloop:
	for {
		select {
		case req, ok := <-c.requests:
			if !ok {
				break reqloop
			}
			c.processReq(req)

		case <-c.baseCtx.Done():
			break reqloop
		}
	}
}

func (c *Channel) processReq(req *ssh.Request) {
	ok := false
	var payloadBuf *bytes.Buffer

	if req.WantReply {
		payloadBuf = &bytes.Buffer{}
		defer func() {
			req.Reply(ok, payloadBuf.Bytes())
		}()
	}

	switch req.Type {
	case "subsystem":
		subsystem, _, err := parseString(req.Payload)
		if err != nil {
			msgLogError(req.WantReply, payloadBuf,
				"failed to find the subsystem requested", err)
			return
		}

		if subsystem != "sftp" {
			msgLogError(req.WantReply, payloadBuf, "unsupported system", errors.New(subsystem))
			return
		}

		sftpserver, err := sftp.NewServer(c.channel)
		if err != nil {
			msgLogError(req.WantReply, payloadBuf,
				"failed to create sftp server over channel", err)
			return
		}

		ok = true

		c.sftpServer = sftpserver

		c.wg.Add(1)

		go func() {
			defer c.wg.Done()
			defer c.channel.Close()
			if err := sftpserver.Serve(); err != nil {
				log.Info("error during sftp session", "err", err.Error())
			}
		}()

	case "pty-req":
		_, parsed, err := parseString(req.Payload)
		if err != nil {
			msgLogError(req.WantReply, payloadBuf, "failed to parse terminfo", err)
			return
		}

		cols, rows, _, _, err := parseWindowSize(req.Payload[parsed:])
		if err != nil {
			msgLogError(req.WantReply, payloadBuf,
				"failed to parse window size", err)
			return
		}

		pty, tty, err := pty.Open()
		if err != nil {
			msgLogError(req.WantReply, payloadBuf,
				"failed to create new pty", err)
			return
		}

		c.pty = pty
		c.tty = tty

		if err := setWindowSize(int(c.pty.Fd()), uint16(rows), uint16(cols)); err != nil {
			log.Info("failed to set window size", "err", err.Error())
		}

		ok = true

	case "window-change":
		if c.pty == nil {
			msgLogError(req.WantReply, payloadBuf, "cannot setup pty", errors.New("pty is not setup"))
			return
		}

		cols, rows, _, _, err := parseWindowSize(req.Payload)
		if err != nil {
			msgLogError(req.WantReply, payloadBuf, "failed to parse window size", err)
			return
		}

		if err := setWindowSize(int(c.pty.Fd()), uint16(rows), uint16(cols)); err != nil {
			msgLogError(req.WantReply, payloadBuf, "failed to set window size", err)
			return
		}

		ok = true

	case "env":
		envname, consumed, err := parseString(req.Payload)
		if err != nil {
			msgLogError(req.WantReply, payloadBuf, "failed to get environment name", err)
			return
		}

		envvalue, _, err := parseString(req.Payload[consumed:])
		if err != nil {
			msgLogError(req.WantReply, payloadBuf, "failed to get environment value", err)
			return
		}

		c.env = append(c.env, fmt.Sprintf("%s=%s", envname, envvalue))

		ok = true

	case "shell":
		if len(req.Payload) > 0 {
			msgLogError(req.WantReply, payloadBuf, "shell doesn't accept payload", errors.New(string(req.Payload)))
			return
		}

		if c.pty == nil {
			msgLogError(req.WantReply, payloadBuf, "pty is not yet setup", errors.New("pty is not yet setup"))
			return
		}

		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.ttyCmd("bash")
		}()

		ok = true

	case "exec":

		commands := make([]string, 0, 16)
		commands = append(commands, "-c")
		payload := req.Payload
		for len(payload) > 0 {
			cmd, parsed, err := parseString(payload)
			if err != nil {
				msgLogError(req.WantReply, payloadBuf, "failed to parse command", err)
				return
			}

			commands = append(commands, cmd)

			payload = payload[parsed:]
		}

		if len(commands) <= 1 {
			msgLogError(req.WantReply, payloadBuf, "no commands in exec", errors.New(string(req.Payload)))
			return
		}

		ok = true

		c.wg.Add(1)

		go func() {
			defer c.wg.Done()
			if c.tty == nil {
				c.noTtyCmd("bash", commands...)
			} else {
				c.ttyCmd("bash", commands...)
			}
		}()

	default:
		msgLogError(req.WantReply, payloadBuf, "unsupported req type", errors.New(req.Type))
		return
	}
}

func msgLogError(wantReplay bool, payloadBuf *bytes.Buffer, msg string, err error) {
	log.Error(msg, "err", err.Error())
	if wantReplay {
		fmt.Fprintf(payloadBuf, "%s: %s", msg, err.Error())
	}
}

func (c *Channel) finishCmd(cmd *exec.Cmd) {
	if err := cmd.Wait(); err != nil {
		log.Error("error in waiting for a process to finish", "err", err.Error())
	}

	if err := c.channel.CloseWrite(); err != nil {
		log.Error("error in closing channel write", "err", err.Error())
	}

	exitcode := uint32(255)
	if cmd.ProcessState != nil {
		exitcode = uint32(cmd.ProcessState.ExitCode())
	}

	if _, err := c.channel.SendRequest(
		"exit-status",
		false,
		binary.BigEndian.AppendUint32(nil, exitcode)); err != nil {
		log.Error("failed to send exit code to remote", "err", err.Error())
	}

	if err := c.channel.Close(); err != nil {
		log.Error("error in closing channel", "err", err.Error())
	}
}

func (c *Channel) ttyCmd(cmd string, args ...string) {
	torun := exec.Command(cmd, args...)

	torun.ExtraFiles = []*os.File{c.tty}
	torun.Stdout = c.tty
	torun.Stderr = c.tty
	torun.Stdin = c.tty

	torun.Dir = c.user.HomeDir
	torun.Env = append(
		torun.Env,
		fmt.Sprintf("USER=%s", c.user.Username),
		fmt.Sprintf("HOME=%s", c.user.HomeDir))
	torun.Env = append(torun.Env, c.env...)

	torun.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    3,
	}

	defer c.finishCmd(torun)

	waiter := make(chan struct{})
	defer func() {
		if err := c.tty.Close(); err != nil {
			log.Info("error in closing tty", "err", err.Error())
		}

		<-waiter
	}()

	if err := torun.Start(); err != nil {
		log.Error("failed to start command", "err", err.Error(), "cmd", cmd)
		return
	}

	go func() {
		defer func() {
			select {
			case <-waiter:
			default:
				close(waiter)
			}
		}()

		_, _ = io.Copy(c.pty, c.channel)
	}()

	go func() {
		defer func() {
			select {
			case <-waiter:
			default:
				close(waiter)
			}
		}()

		_, _ = io.Copy(c.channel, c.pty)
	}()
}

func (c *Channel) noTtyCmd(cmd string, args ...string) {
	torun := exec.Command(cmd, args...)

	torun.Stdout = c.channel
	torun.Stderr = c.channel

	defer c.finishCmd(torun)

	if err := torun.Start(); err != nil {
		log.Error("failed to start command", "err", err.Error(), "cmd", cmd)
		return
	}
}
