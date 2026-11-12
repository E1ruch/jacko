package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"

	"jacko/server/internal/accounts"
)

type Mailer struct {
	address, host, from, user, password string
	local                               bool
}

func New(address, from, user, password string) (*Mailer, error) {
	host, _, err := net.SplitHostPort(address)
	sender, parseErr := mail.ParseAddress(from)
	if err != nil || host == "" || parseErr != nil || sender.Address != from || ((user == "") != (password == "")) {
		return nil, errors.New("invalid SMTP configuration")
	}
	ip := net.ParseIP(host)
	return &Mailer{address: address, host: host, from: from, user: user, password: password,
		local: (host == "localhost" || (ip != nil && ip.IsLoopback())) && user == ""}, nil
}

func (m *Mailer) Send(ctx context.Context, recipient string, purpose accounts.Purpose, code string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", m.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	} else if !m.local {
		return errors.New("SMTP server does not offer STARTTLS")
	}
	if m.user != "" {
		if err := client.Auth(smtp.PlainAuth("", m.user, m.password, m.host)); err != nil {
			return err
		}
	}
	if err := client.Mail(m.from); err != nil {
		return err
	}
	if err := client.Rcpt(recipient); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	subject := "Jacko email confirmation"
	if purpose == accounts.ResetPassword {
		subject = "Jacko password reset"
	}
	if _, err := fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nCode: %s\r\n", m.from, recipient, subject, code); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
