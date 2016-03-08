package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cactus/go-statsd-client/statsd"

	"github.com/letsencrypt/boulder/cmd"
	blog "github.com/letsencrypt/boulder/log"
)

var apiPort = flag.String("http", "9381", "http port to listen on")

type rcvdMail struct {
	From string
	To   string
	Mail string
}

var allReceivedMail []rcvdMail
var allMailMutex sync.Mutex

func expectLine(buf *bufio.Reader, expected string) error {
	line, _, err := buf.ReadLine()
	if err != nil {
		return fmt.Errorf("readline: %v", err)
	}
	if string(line) != expected {
		return fmt.Errorf("Expected %s, got %s", expected, line)
	}
	return nil
}

func getLine(buf *bufio.Reader) (string, error) {
	line, _, err := buf.ReadLine()
	if err != nil {
		return "", fmt.Errorf("readline: %v", err)
	}
	return string(line), nil
}

var mailFromRegex = regexp.MustCompile("^MAIL FROM:<(.*)>\\s*BODY=8BITMIME\\s*$")
var rcptToRegex = regexp.MustCompile("^RCPT TO:<(.*)>\\s*$")
var dataRegex = regexp.MustCompile("^DATA\\s*$")
var smtpErr501 = []byte("501 syntax error in parameters or arguments \r\n")
var smtpOk250 = []byte("250 OK \r\n")
var authLine string

func handleConn(conn net.Conn) {
	defer conn.Close()
	auditlogger := blog.GetAuditLogger()
	auditlogger.Info(fmt.Sprintf("mailcatch: Got connection from %s", conn.RemoteAddr()))

	readBuf := bufio.NewReader(conn)
	conn.Write([]byte("220 smtp.example.com ESMTP\r\n"))
	if err := expectLine(readBuf, "EHLO localhost"); err != nil {
		log.Printf("mailcatch: %s: %v\n", conn.RemoteAddr(), err)
		return
	}
	conn.Write([]byte("250-PIPELINING\r\n"))
	conn.Write([]byte("250-AUTH PLAIN LOGIN\r\n"))
	conn.Write([]byte("250 8BITMIME\r\n"))
	if err := expectLine(readBuf, authLine); err != nil {
		log.Printf("mailcatch: %s: %v\n", conn.RemoteAddr(), err)
		return
	}
	conn.Write([]byte("235 2.7.0 Authentication successful\r\n"))
	auditlogger.Info(fmt.Sprintf("mailcatch: Successful auth from %s", conn.RemoteAddr()))
	// == END authentication

	// necessary commands:
	// MAIL RCPT DATA QUIT

	var fromAddr string
	var toAddr []string
	var msgBuf bytes.Buffer

	clearState := func() {
		fromAddr = ""
		toAddr = nil
		msgBuf = bytes.Buffer{}
	}

	var quit = false
	for !quit {
		line, err := getLine(readBuf)
		if err != nil {
			log.Printf("mailcatch: %s: getline: %v\n", conn.RemoteAddr(), err)
			return
		}

		cmdSplit := strings.SplitN(line, " ", 2)
		cmd := cmdSplit[0]
		switch cmd {
		case "QUIT":
			quit = true
			conn.Write([]byte("221 Bye \r\n"))
			break
		case "RSET":
			clearState()
			conn.Write(smtpOk250)
		case "NOOP":
			conn.Write(smtpOk250)
		case "MAIL":
			clearState()
			matches := mailFromRegex.FindStringSubmatch(line)
			if matches == nil {
				log.Printf("mailcatch: %s: MAIL FROM parse error\n", conn.RemoteAddr())
				conn.Write(smtpErr501)
				continue
			}
			addr, err := mail.ParseAddress(matches[1])
			if err != nil {
				log.Printf("mailcatch: %s: addr parse error: %v\n", conn.RemoteAddr(), err)
				conn.Write(smtpErr501)
				continue
			}
			fromAddr = addr.Address
			conn.Write(smtpOk250)
		case "RCPT":
			matches := rcptToRegex.FindStringSubmatch(line)
			if matches == nil {
				conn.Write(smtpErr501)
				continue
			}
			addr, err := mail.ParseAddress(matches[1])
			if err != nil {
				log.Printf("mailcatch: %s: addr parse error: %v\n", conn.RemoteAddr(), err)
				conn.Write(smtpErr501)
				continue
			}
			toAddr = append(toAddr, addr.Address)
			conn.Write(smtpOk250)
		case "DATA":
			conn.Write([]byte("354 Start mail input \r\n"))

			for {
				line, err := getLine(readBuf)
				if err != nil {
					log.Printf("mailcatch: %s: getline: %v\n", conn.RemoteAddr(), err)
					return
				}
				msgBuf.WriteString(line)
				msgBuf.WriteString("\r\n")
				if strings.HasSuffix(msgBuf.String(), "\r\n.\r\n") {
					break
				}
			}

			mailResult := rcvdMail{
				From: fromAddr,
				Mail: msgBuf.String(),
			}
			allMailMutex.Lock()
			for _, rcpt := range toAddr {
				mailResult.To = rcpt
				allReceivedMail = append(allReceivedMail, mailResult)
				log.Printf("mailcatch: Got mail: %s -> %s\n", fromAddr, rcpt)
			}
			allMailMutex.Unlock()
			conn.Write([]byte("250 Got mail \r\n"))
			clearState()
		}
	}
}

func serveSMTP(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn)
	}
}

func calculateAuth(c cmd.Config) error {
	var buf bytes.Buffer
	enc := base64.NewEncoder(base64.StdEncoding, &buf)
	enc.Write([]byte{0})
	enc.Write([]byte(c.Mailer.Username))
	enc.Write([]byte{0})
	smtpPassword, err := c.Mailer.PasswordConfig.Pass()
	if err != nil {
		return err
	}
	enc.Write([]byte(smtpPassword))
	enc.Close()
	authLine = fmt.Sprintf("AUTH PLAIN %s", buf.String())
	return nil
}

func main() {
	app := cmd.NewAppShell("mailcatch", "A simple SMTP server")
	app.Action = func(c cmd.Config, stats statsd.Statter, auditlogger *blog.AuditLogger) {
		err := calculateAuth(c)
		cmd.FailOnError(err, "Couldn't calculate password")

		l, err := net.Listen("tcp", ":"+c.Mailer.Port)
		cmd.FailOnError(err, "Couldn't bind for SMTP")
		defer l.Close()

		setupHTTP(http.DefaultServeMux)
		go func() {
			err := http.ListenAndServe(":"+*apiPort, http.DefaultServeMux)
			cmd.FailOnError(err, "Couldn't start HTTP server")
		}()

		err = serveSMTP(l)
		cmd.FailOnError(err, "Failed to accept connection")
	}

	app.Run()
}
