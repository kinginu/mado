package imap

import (
	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

var imapclientBuf = imapclient.FetchMessageBuffer{UID: 5, Envelope: &goimap.Envelope{MessageID: "a+b@example.org", Subject: "s"}}
