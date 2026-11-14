package browser

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

const (
	portalName      = "org.freedesktop.portal.Desktop"
	portalObject    = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	requestPrefix   = "/org/freedesktop/portal/desktop/request/"
	requestClose    = "org.freedesktop.portal.Request.Close"
	requestOpenURI  = "org.freedesktop.portal.OpenURI.OpenURI"
	requestResponse = "org.freedesktop.portal.Request"
)

type portalConnection struct {
	conn       *dbus.Conn
	socket     *portalSocket
	signals    *portalSignals
	stopCancel func() bool
	cancelDone chan struct{}
	cancelErr  error
	owner      string
	handle     dbus.ObjectPath
	issued     bool
	completed  bool
}

func openPortal(ctx context.Context, environment []string, uri string) (result error) {
	address, err := socketAddress(environment)
	if err != nil {
		return err
	}
	client, err := connectPortal(ctx, address)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, client.close(ctx)) }()
	return client.open(ctx, uri)
}

func connectPortal(ctx context.Context, address string) (*portalConnection, error) {
	if ctx == nil {
		return nil, errActivation
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, errActivation
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return nil, err
	}
	client, err := configurePortalConnection(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err = client.conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err == nil {
		err = client.conn.Hello()
	}
	if err != nil {
		return nil, errors.Join(err, client.close(ctx))
	}
	return client, nil
}

func configurePortalConnection(ctx context.Context, raw net.Conn) (*portalConnection, error) {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return nil, errors.Join(errActivation, raw.Close())
	}
	socket := &portalSocket{Conn: raw}
	if err := socket.SetDeadline(deadline); err != nil {
		return nil, errors.Join(err, socket.Close())
	}
	signals := newPortalSignals()
	conn, err := dbus.NewConn(socket, dbus.WithContext(ctx), dbus.WithSignalHandler(signals))
	if err != nil {
		return nil, errors.Join(err, socket.Close())
	}
	client := &portalConnection{conn: conn, socket: socket, signals: signals, cancelDone: make(chan struct{})}
	client.stopCancel = context.AfterFunc(ctx, func() {
		// Close caches its error; owner cleanup joins it after this callback.
		client.cancelErr = socket.Close()
		close(client.cancelDone)
	})
	return client, nil
}

func (c *portalConnection) open(ctx context.Context, uri string) error {
	owner, err := portalOwner(ctx, c.conn)
	if err != nil {
		return err
	}
	token, err := randomid.NewRandomID()
	if err != nil {
		return err
	}
	names := c.conn.Names()
	if len(names) == 0 || !strings.HasPrefix(names[0], ":") {
		return errActivation
	}
	sender := strings.ReplaceAll(strings.TrimPrefix(names[0], ":"), ".", "_")
	c.owner, c.handle = owner, dbus.ObjectPath(requestPrefix+sender+"/delegate_"+token)
	if !c.handle.IsValid() {
		return errActivation
	}
	c.signals.selectRequest(owner, c.handle)
	if err = c.conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(owner), dbus.WithMatchObjectPath(c.handle), dbus.WithMatchInterface(requestResponse), dbus.WithMatchMember("Response")); err != nil {
		return err
	}
	c.issued = true
	var handle dbus.ObjectPath
	options := map[string]dbus.Variant{"handle_token": dbus.MakeVariant("delegate_" + token)}
	if err = c.conn.Object(owner, portalObject).CallWithContext(ctx, requestOpenURI, 0, "", uri, options).Store(&handle); err != nil {
		return err
	}
	if handle != c.handle {
		return errActivation
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case signal := <-c.signals.response:
		c.completed, err = responseResult(signal)
		return err
	}
}

func portalOwner(ctx context.Context, conn *dbus.Conn) (string, error) {
	var owner string
	err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, portalName).Store(&owner)
	var remote dbus.Error
	if errors.As(err, &remote) && remote.Name == "org.freedesktop.DBus.Error.NameHasNoOwner" {
		var disposition uint32
		err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.StartServiceByName", 0, portalName, uint32(0)).Store(&disposition)
		if err == nil && (disposition == 1 || disposition == 2) {
			err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, portalName).Store(&owner)
		}
	}
	if err != nil || !strings.HasPrefix(owner, ":") || len(owner) > 255 {
		return "", errors.Join(errActivation, err)
	}
	return owner, nil
}

func (c *portalConnection) close(ctx context.Context) error {
	var requestErr error
	if c.issued && !c.completed && ctx.Err() == nil {
		requestErr = c.conn.Object(c.owner, c.handle).CallWithContext(ctx, requestClose, 0).Err
	}
	// Raw close precedes Conn.Close: godbus holds its output lock across writes.
	// Closing only Conn while a write stalls can otherwise deadlock cleanup.
	rawErr := c.socket.Close()
	connErr := c.conn.Close()
	if !c.stopCancel() {
		<-c.cancelDone
	}
	return errors.Join(requestErr, rawErr, connErr, c.cancelErr)
}

type portalSocket struct {
	net.Conn
	once sync.Once
	err  error
}

func (s *portalSocket) Close() error {
	s.once.Do(func() { s.err = s.Conn.Close() })
	return s.err
}
