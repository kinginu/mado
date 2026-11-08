// Package all registers every inbox kind. A new kind is one package under
// internal/inbox/<kind> that calls inbox.Register in init(), plus one import here.
package all

import (
	_ "github.com/kinginu/mado/internal/inbox/github"
	_ "github.com/kinginu/mado/internal/inbox/imap"
	_ "github.com/kinginu/mado/internal/inbox/push"
	_ "github.com/kinginu/mado/internal/inbox/rss"
	_ "github.com/kinginu/mado/internal/inbox/slack"
)
