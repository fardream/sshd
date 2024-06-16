package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/user"
	"sync"

	"golang.org/x/crypto/ssh"
)

// ServerConn is a ssh server connection
type ServerConn struct {
	// sshcon is the underlying connection
	sshcon *ssh.ServerConn

	// newchanchan delivers the new channel request
	newchanchan <-chan ssh.NewChannel

	baseCtx    context.Context
	baseCancel context.CancelFunc

	chans []*Channel

	wg sync.WaitGroup

	user *user.User
}

func NewFromConn(ctx context.Context, conn net.Conn, config *ssh.ServerConfig) (*ServerConn, error) {
	sshconn, newchanchan, request, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new connection: %w", err)
	}

	user, err := user.Lookup(sshconn.User())
	if err != nil {
		sshconn.Close()
		return nil, fmt.Errorf("cannot find user %s: %w", sshconn.User(), err)
	}

	go ssh.DiscardRequests(request)

	baseCtx, baseCancel := context.WithCancel(ctx)

	s := &ServerConn{
		sshcon:      sshconn,
		newchanchan: newchanchan,
		baseCtx:     baseCtx,
		baseCancel:  baseCancel,
		user:        user,
	}

	return s, nil
}

// Wait for all the long running sessions such as terminal, exec to finish.
func (s *ServerConn) Wait() {
	s.wg.Wait()
}

// Close tear the connection down
func (s *ServerConn) Close() error {
	s.Wait()

	errs := make([]error, 0, len(s.chans)*3)

	for _, channel := range s.chans {
		channel.baseCancel()

		if channel.pty != nil {
			errs = append(errs, channel.pty.Close())
		}
		if channel.tty != nil {
			errs = append(errs, channel.tty.Close())
		}
		if channel.channel != nil {
			errs = append(errs, channel.channel.Close())
		}
	}

	s.baseCancel()

	errs = append(errs, s.sshcon.Close())

	return errors.Join(errs...)
}

func (s *ServerConn) Loop() {
	defer s.sshcon.Wait()

serverloop:
	for {
		select {
		case newchan, ok := <-s.newchanchan:
			if !ok {
				break serverloop
			}

			s.procesNewChan(newchan)

		case <-s.baseCtx.Done():
			break serverloop
		}
	}
}

func (s *ServerConn) procesNewChan(newchannel ssh.NewChannel) {
	channeltype := newchannel.ChannelType()

	if channeltype != "session" {
		newchannel.Reject(ssh.UnknownChannelType, channeltype)
		return
	}

	channel, requests, err := newchannel.Accept()
	if err != nil {
		slog.Info("failed to accept channel", "err", err.Error())
	}

	basectx, basecancel := context.WithCancel(s.baseCtx)

	c := &Channel{
		channel:    channel,
		requests:   requests,
		env:        nil,
		tty:        nil,
		pty:        nil,
		baseCtx:    basectx,
		baseCancel: basecancel,
		wg:         &s.wg,
		user:       s.user,
	}

	s.chans = append(s.chans, c)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		c.Loop()
	}()

	return
}
