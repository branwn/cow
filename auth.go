package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/cyfdecyf/bufio"
)

const (
	authRealm       = "cow proxy"
	authRawBodyTmpl = `<!DOCTYPE html>
<html>
	<head> <title>COW Proxy</title> </head>
	<body>
		<h1>407 Proxy authentication required</h1>
		<hr />
		Generated by <i>COW</i>
	</body>
</html>
`
)

type netAddr struct {
	ip   net.IP
	mask net.IPMask
}

type authUser struct {
	// user name is the key to auth.user, no need to store here
	passwd string
	ha1    string // used in request digest, initialized ondemand
	port   uint16 // 0 means any port
}

var auth struct {
	required bool

	user map[string]*authUser

	allowedClient []netAddr

	authed *TimeoutSet // cache authenticated users based on ip

	template *template.Template
}

func (au *authUser) initHA1(user string) {
	if au.ha1 == "" {
		au.ha1 = md5sum(user + ":" + authRealm + ":" + au.passwd)
	}
}

func parseUserPasswd(userPasswd string) (user string, au *authUser, err error) {
	arr := strings.Split(userPasswd, ":")
	n := len(arr)
	if n == 1 || n > 3 {
		err = errors.New("user password: " + userPasswd +
			" syntax wrong, should be username:password[:port]")
		return
	}
	user, passwd := arr[0], arr[1]
	if user == "" || passwd == "" {
		err = errors.New("user password " + userPasswd +
			" should not contain empty user name or password")
		return "", nil, err
	}
	var port int
	if n == 3 && arr[2] != "" {
		port, err = strconv.Atoi(arr[2])
		if err != nil || port <= 0 || port > 0xffff {
			err = errors.New("user password: " + userPasswd + " invalid port")
			return "", nil, err
		}
	}
	au = &authUser{passwd, "", uint16(port)}
	return user, au, nil
}

func parseAllowedClient(val string) {
	if val == "" {
		return
	}
	arr := strings.Split(val, ",")
	auth.allowedClient = make([]netAddr, len(arr))
	for i, v := range arr {
		s := strings.TrimSpace(v)
		ipAndMask := strings.Split(s, "/")
		if len(ipAndMask) > 2 {
			Fatal("allowedClient syntax error: client should be the form ip/nbitmask")
		}
		ip := net.ParseIP(ipAndMask[0])
		if ip == nil {
			Fatalf("allowedClient syntax error %s: ip address not valid\n", s)
		}
		var mask net.IPMask
		if len(ipAndMask) == 2 {
			nbit, err := strconv.Atoi(ipAndMask[1])
			if err != nil {
				Fatalf("allowedClient syntax error %s: %v\n", s, err)
			}
			if nbit > 32 {
				Fatal("allowedClient error: mask number should <= 32")
			}
			mask = NewNbitIPv4Mask(nbit)
		} else {
			mask = NewNbitIPv4Mask(32)
		}
		auth.allowedClient[i] = netAddr{ip.Mask(mask), mask}
	}
}

func addUserPasswd(val string) {
	if val == "" {
		return
	}
	user, au, err := parseUserPasswd(val)
	debug.Println("user:", user, "port:", au.port)
	if err != nil {
		Fatal(err)
	}
	if _, ok := auth.user[user]; ok {
		Fatal("duplicate user:", user)
	}
	auth.user[user] = au
}

func loadUserPasswdFile(file string) {
	if file == "" {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		Fatal("error opening user passwd fle:", err)
	}

	r := bufio.NewReader(f)
	s := bufio.NewScanner(r)
	for s.Scan() {
		addUserPasswd(s.Text())
	}
	f.Close()
}

func initAuth() {
	if config.UserPasswd != "" ||
		config.UserPasswdFile != "" ||
		config.AllowedClient != "" {
		auth.required = true
	} else {
		return
	}

	auth.user = make(map[string]*authUser)

	addUserPasswd(config.UserPasswd)
	loadUserPasswdFile(config.UserPasswdFile)
	parseAllowedClient(config.AllowedClient)

	auth.authed = NewTimeoutSet(time.Duration(config.AuthTimeout) * time.Hour)

	rawTemplate := "HTTP/1.1 407 Proxy Authentication Required\r\n" +
		"Proxy-Authenticate: Digest realm=\"" + authRealm + "\", nonce=\"{{.Nonce}}\", qop=\"auth\"\r\n" +
		"Content-Type: text/html\r\n" +
		"Cache-Control: no-cache\r\n" +
		"Content-Length: " + fmt.Sprintf("%d", len(authRawBodyTmpl)) + "\r\n\r\n" + authRawBodyTmpl
	var err error
	if auth.template, err = template.New("auth").Parse(rawTemplate); err != nil {
		Fatal("internal error generating auth template:", err)
	}
}

// Return err = nil if authentication succeed. nonce would be not empty if
// authentication is needed, and should be passed back on subsequent call.
func Authenticate(conn *clientConn, r *Request) (err error) {
	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	if auth.authed.has(clientIP) {
		debug.Printf("%s has already authed\n", clientIP)
		return
	}
	if authIP(clientIP) { // IP is allowed
		return
	}
	err = authUserPasswd(conn, r)
	if err == nil {
		auth.authed.add(clientIP)
	}
	return
}

// authIP checks whether the client ip address matches one in allowedClient.
// It uses a sequential search.
func authIP(clientIP string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		panic("authIP should always get IP address")
	}

	for _, na := range auth.allowedClient {
		if ip.Mask(na.mask).Equal(na.ip) {
			debug.Printf("client ip %s allowed\n", clientIP)
			return true
		}
	}
	return false
}

func genNonce() string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%x", time.Now().Unix())
	return buf.String()
}

func calcRequestDigest(kv map[string]string, ha1, method string) string {
	// Refer to rfc2617 section 3.2.2.1 Request-Digest
	arr := []string{
		ha1,
		kv["nonce"],
		kv["nc"],
		kv["cnonce"],
		"auth",
		md5sum(method + ":" + kv["uri"]),
	}
	return md5sum(strings.Join(arr, ":"))
}

func checkProxyAuthorization(conn *clientConn, r *Request) error {
	if debug {
		debug.Printf("cli(%s) authorization: %s\n", conn.RemoteAddr(), r.ProxyAuthorization)
	}

	arr := strings.SplitN(r.ProxyAuthorization, " ", 2)
	if len(arr) != 2 {
		return errors.New("auth: malformed ProxyAuthorization header: " + r.ProxyAuthorization)
	}
	authMethod := strings.ToLower(strings.TrimSpace(arr[0]))
	if authMethod == "digest" {
		return authDigest(conn, r, arr[1])
	} else if authMethod == "basic" {
		return authBasic(conn, arr[1])
	}
	return errors.New("auth: method " + arr[0] + " unsupported, must use digest")
}

func authPort(conn *clientConn, user string, au *authUser) error {
	if au.port == 0 {
		return nil
	}
	_, portStr, _ := net.SplitHostPort(conn.LocalAddr().String())
	port, _ := strconv.Atoi(portStr)
	if uint16(port) != au.port {
		errl.Printf("cli(%s) auth: user %s port not match\n", conn.RemoteAddr(), user)
		return errAuthRequired
	}
	return nil
}

func authBasic(conn *clientConn, userPasswd string) error {
	b64, err := base64.StdEncoding.DecodeString(userPasswd)
	if err != nil {
		return errors.New("auth:" + err.Error())
	}
	arr := strings.Split(string(b64), ":")
	if len(arr) != 2 {
		return errors.New("auth: malformed basic auth user:passwd")
	}
	user := arr[0]
	passwd := arr[1]

	au, ok := auth.user[user]
	if !ok || au.passwd != passwd {
		return errAuthRequired
	}
	return authPort(conn, user, au)
}

func authDigest(conn *clientConn, r *Request, keyVal string) error {
	authHeader := parseKeyValueList(keyVal)
	if len(authHeader) == 0 {
		return errors.New("auth: empty authorization list")
	}
	nonceTime, err := strconv.ParseInt(authHeader["nonce"], 16, 64)
	if err != nil {
		return fmt.Errorf("auth: nonce %v", err)
	}
	// If nonce time too early, reject. iOS will create a new connection to do
	// authentication.
	if time.Now().Sub(time.Unix(nonceTime, 0)) > time.Minute {
		return errAuthRequired
	}

	user := authHeader["username"]
	au, ok := auth.user[user]
	if !ok {
		errl.Printf("cli(%s) auth: no such user: %s\n", conn.RemoteAddr(), authHeader["username"])
		return errAuthRequired
	}

	if err = authPort(conn, user, au); err != nil {
		return err
	}
	if authHeader["qop"] != "auth" {
		return errors.New("auth: qop wrong: " + authHeader["qop"])
	}
	response, ok := authHeader["response"]
	if !ok {
		return errors.New("auth: no request-digest response")
	}

	au.initHA1(user)
	digest := calcRequestDigest(authHeader, au.ha1, r.Method)
	if response != digest {
		errl.Printf("cli(%s) auth: digest not match, maybe password wrong", conn.RemoteAddr())
		return errAuthRequired
	}
	return nil
}

func authUserPasswd(conn *clientConn, r *Request) (err error) {
	if r.ProxyAuthorization != "" {
		// client has sent authorization header
		err = checkProxyAuthorization(conn, r)
		if err == nil {
			return
		} else if err != errAuthRequired {
			sendErrorPage(conn, statusBadReq, "Bad authorization request", err.Error())
			return
		}
		// auth required to through the following
	}

	nonce := genNonce()
	data := struct {
		Nonce string
	}{
		nonce,
	}
	buf := new(bytes.Buffer)
	if err := auth.template.Execute(buf, data); err != nil {
		return fmt.Errorf("error generating auth response: %v", err)
	}
	if bool(debug) && verbose {
		debug.Printf("authorization response:\n%s", buf.String())
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("send auth response error: %v", err)
	}
	return errAuthRequired
}
