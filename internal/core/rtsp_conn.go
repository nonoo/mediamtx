package core

import (
	"errors"
	"fmt"
	"net"
	"time"
	"strings"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/auth"
	"github.com/bluenviron/gortsplib/v3/pkg/base"
	"github.com/bluenviron/gortsplib/v3/pkg/headers"
	"github.com/bluenviron/gortsplib/v3/pkg/url"
	"github.com/google/uuid"

	"github.com/aler9/mediamtx/internal/conf"
	"github.com/aler9/mediamtx/internal/externalcmd"
	"github.com/aler9/mediamtx/internal/logger"
)

const (
	rtspConnPauseAfterAuthError = 2 * time.Second
)

type rtspConnParent interface {
	log(logger.Level, string, ...interface{})
}

type rtspConn struct {
	externalAuthenticationURL string
	rtspAddress               string
	defaultPath               string
	authMethods               []headers.AuthMethod
	readTimeout               conf.StringDuration
	runOnConnect              string
	runOnConnectRestart       bool
	externalCmdPool           *externalcmd.Pool
	pathManager               *pathManager
	conn                      *gortsplib.ServerConn
	parent                    rtspConnParent

	uuid          uuid.UUID
	created       time.Time
	onConnectCmd  *externalcmd.Cmd
	authUser      string
	authPass      string
	authValidator *auth.Validator
	authFailures  int
}

func newRTSPConn(
	externalAuthenticationURL string,
	rtspAddress string,
	defaultPath string,
	authMethods []headers.AuthMethod,
	readTimeout conf.StringDuration,
	runOnConnect string,
	runOnConnectRestart bool,
	externalCmdPool *externalcmd.Pool,
	pathManager *pathManager,
	conn *gortsplib.ServerConn,
	parent rtspConnParent,
) *rtspConn {
	c := &rtspConn{
		externalAuthenticationURL: externalAuthenticationURL,
		rtspAddress:               rtspAddress,
		defaultPath:               defaultPath,
		authMethods:               authMethods,
		readTimeout:               readTimeout,
		runOnConnect:              runOnConnect,
		runOnConnectRestart:       runOnConnectRestart,
		externalCmdPool:           externalCmdPool,
		pathManager:               pathManager,
		conn:                      conn,
		parent:                    parent,
		uuid:                      uuid.New(),
		created:                   time.Now(),
	}

	c.log(logger.Info, "opened")

	if c.runOnConnect != "" {
		c.log(logger.Info, "runOnConnect command started")
		_, port, _ := net.SplitHostPort(c.rtspAddress)
		c.onConnectCmd = externalcmd.NewCmd(
			c.externalCmdPool,
			c.runOnConnect,
			c.runOnConnectRestart,
			externalcmd.Environment{
				"RTSP_PATH": "",
				"RTSP_PORT": port,
			},
			func(co int) {
				c.log(logger.Info, "runOnInit command exited with code %d", co)
			})
	}

	return c
}

func (c *rtspConn) log(level logger.Level, format string, args ...interface{}) {
	c.parent.log(level, "[conn %v] "+format, append([]interface{}{c.conn.NetConn().RemoteAddr()}, args...)...)
}

// Conn returns the RTSP connection.
func (c *rtspConn) Conn() *gortsplib.ServerConn {
	return c.conn
}

func (c *rtspConn) remoteAddr() net.Addr {
	return c.conn.NetConn().RemoteAddr()
}

func (c *rtspConn) ip() net.IP {
	return c.conn.NetConn().RemoteAddr().(*net.TCPAddr).IP
}

func (c *rtspConn) authenticate(
	path string,
	query string,
	pathIPs []fmt.Stringer,
	pathUser conf.Credential,
	pathPass conf.Credential,
	isPublishing bool,
	req *base.Request,
	baseURL *url.URL,
) error {
	if c.externalAuthenticationURL != "" {
		username := ""
		password := ""

		var auth headers.Authorization
		err := auth.Unmarshal(req.Header["Authorization"])
		if err == nil && auth.Method == headers.AuthBasic {
			username = auth.BasicUser
			password = auth.BasicPass
		}

		err = externalAuth(
			c.externalAuthenticationURL,
			c.ip().String(),
			username,
			password,
			path,
			externalAuthProtoRTSP,
			&c.uuid,
			isPublishing,
			query)
		if err != nil {
			c.authFailures++

			// VLC with login prompt sends 4 requests:
			// 1) without credentials
			// 2) with password but without username
			// 3) without credentials
			// 4) with password and username
			// therefore we must allow up to 3 failures
			if c.authFailures > 3 {
				return pathErrAuthCritical{
					message: "unauthorized: " + err.Error(),
					response: &base.Response{
						StatusCode: base.StatusUnauthorized,
					},
				}
			}

			v := "IPCAM"
			return pathErrAuthNotCritical{
				message: "unauthorized: " + err.Error(),
				response: &base.Response{
					StatusCode: base.StatusUnauthorized,
					Header: base.Header{
						"WWW-Authenticate": headers.Authenticate{
							Method: headers.AuthBasic,
							Realm:  &v,
						}.Marshal(),
					},
				},
			}
		}
	}

	if pathIPs != nil {
		ip := c.ip()
		if !ipEqualOrInRange(ip, pathIPs) {
			return pathErrAuthCritical{
				message: fmt.Sprintf("IP '%s' not allowed", ip),
				response: &base.Response{
					StatusCode: base.StatusUnauthorized,
				},
			}
		}
	}

	if pathUser != "" {
		// reset authValidator every time the credentials change
		if c.authValidator == nil || c.authUser != string(pathUser) || c.authPass != string(pathPass) {
			c.authUser = string(pathUser)
			c.authPass = string(pathPass)
			c.authValidator = auth.NewValidator(string(pathUser), string(pathPass), c.authMethods)
		}

		err := c.authValidator.ValidateRequest(req, baseURL)
		if err != nil {
			c.authFailures++

			// VLC with login prompt sends 4 requests:
			// 1) without credentials
			// 2) with password but without username
			// 3) without credentials
			// 4) with password and username
			// therefore we must allow up to 3 failures
			if c.authFailures > 3 {
				return pathErrAuthCritical{
					message: "unauthorized: " + err.Error(),
					response: &base.Response{
						StatusCode: base.StatusUnauthorized,
					},
				}
			}

			return pathErrAuthNotCritical{
				response: &base.Response{
					StatusCode: base.StatusUnauthorized,
					Header: base.Header{
						"WWW-Authenticate": c.authValidator.Header(),
					},
				},
			}
		}

		// login successful, reset authFailures
		c.authFailures = 0
	}

	return nil
}

// onClose is called by rtspServer.
func (c *rtspConn) onClose(err error) {
	c.log(logger.Info, "closed (%v)", err)

	if c.onConnectCmd != nil {
		c.onConnectCmd.Close()
		c.log(logger.Info, "runOnConnect command stopped")
	}
}

// onRequest is called by rtspServer.
func (c *rtspConn) onRequest(req *base.Request) {
	c.log(logger.Debug, "[c->s] %v", req)
}

// OnResponse is called by rtspServer.
func (c *rtspConn) OnResponse(res *base.Response) {
	c.log(logger.Debug, "[s->c] %v", res)
}

// onDescribe is called by rtspServer.
func (c *rtspConn) onDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Path == "/" {
		defaultPath := strings.TrimSuffix(c.defaultPath, "/")
		if url, err := url.Parse(ctx.Request.URL.String() + defaultPath); err == nil {
			ctx.Path += defaultPath
			ctx.Request.URL = url
		}
	}

	if len(ctx.Path) == 0 || ctx.Path[0] != '/' {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, nil, fmt.Errorf("invalid path")
	}
	ctx.Path = ctx.Path[1:]

	res := c.pathManager.describe(pathDescribeReq{
		pathName: ctx.Path,
		url:      ctx.Request.URL,
		authenticate: func(
			pathIPs []fmt.Stringer,
			pathUser conf.Credential,
			pathPass conf.Credential,
		) error {
			return c.authenticate(ctx.Path, ctx.Query, pathIPs, pathUser, pathPass, false, ctx.Request, nil)
		},
	})

	if res.err != nil {
		switch terr := res.err.(type) {
		case pathErrAuthNotCritical:
			c.log(logger.Debug, "non-critical authentication error: %s", terr.message)
			return terr.response, nil, nil

		case pathErrAuthCritical:
			// wait some seconds to stop brute force attacks
			<-time.After(rtspConnPauseAfterAuthError)

			return terr.response, nil, errors.New(terr.message)

		case pathErrNoOnePublishing:
			return &base.Response{
				StatusCode: base.StatusNotFound,
			}, nil, res.err

		default:
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, nil, res.err
		}
	}

	if res.redirect != "" {
		return &base.Response{
			StatusCode: base.StatusMovedPermanently,
			Header: base.Header{
				"Location": base.HeaderValue{res.redirect},
			},
		}, nil, nil
	}

	return &base.Response{
		StatusCode: base.StatusOK,
	}, res.stream.rtspStream, nil
}
