package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Shorten the handler's real network deadline, without a production test hook.
// HTTP has no server-wide ReadTimeout; only the login handler can unblock this
// deliberately incomplete JSON body.
type loginDeadlineConn struct{ net.Conn }

func (c loginDeadlineConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() && time.Until(deadline) > 50*time.Millisecond {
		deadline = time.Now().Add(50 * time.Millisecond)
	}
	return c.Conn.SetReadDeadline(deadline)
}

type loginDeadlineListener struct{ net.Listener }

func (l loginDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return loginDeadlineConn{c}, nil
}

func TestBrowserLoginBoundsSlowJSONBody(t *testing.T) {
	server := &Server{}
	engine := gin.New()
	engine.POST("/session", server.handleBrowserLogin)
	endpoint := httptest.NewUnstartedServer(engine)
	endpoint.Listener = loginDeadlineListener{endpoint.Listener}
	endpoint.Start()
	defer endpoint.Close()
	conn, err := net.Dial("tcp", endpoint.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	_, err = fmt.Fprint(conn, "POST /session HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"key\":\"")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "slow JSON must hit the handler deadline, not the test connection deadline")
	defer response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}
