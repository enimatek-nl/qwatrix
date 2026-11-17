package roomserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// RemoteDirectorySource asks another homeserver for the rooms it publishes.
//
// It exists because a room directory is not a global list. Asking this server what rooms exist has
// one answer; asking it what *another* server publishes is a different question, and one a client
// expects to be able to ask at all — Element's explore pane puts a server name in a box and sends it
// here, and a server that quietly answers with its own list looks to that user like a server with no
// rooms on it.
type RemoteDirectorySource interface {
	PublicRooms(ctx context.Context, server string, req PublicRoomsRequest) (*PublicRoomsPage, error)
}

// RemoteAliasSource turns an alias belonging to another server into the room ID it points at.
//
// Without it, `#room:elsewhere` is indistinguishable from a room that does not exist: this server's
// own alias table cannot contain it, and the only thing left is to ask the server the alias names.
type RemoteAliasSource interface {
	LookupAlias(ctx context.Context, alias string) (string, error)
}

// WithRemoteDirectory connects the room directory to the federation transport. Without it another
// server's directory cannot be browsed, and the answer is a clear refusal rather than this server's
// own list passed off as somebody else's answer.
func (s *Service) WithRemoteDirectory(source RemoteDirectorySource) *Service {
	s.remoteDirectory = source
	return s
}

// WithRemoteAliasSource connects alias resolution to the federation transport. Without it a remote
// alias is a 404 from this server, which is a claim about a room this server has no standing to make.
func (s *Service) WithRemoteAliasSource(source RemoteAliasSource) *Service {
	s.remoteAliases = source
	return s
}

// remotePage fetches another server's published list.
func (s *Service) remotePage(ctx context.Context, server string, req PublicRoomsRequest) (*PublicRoomsPage, error) {
	if s.remoteDirectory == nil {
		return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"This server cannot ask %q for its room directory: outbound federation is off", server)
	}
	return s.remoteDirectory.PublicRooms(ctx, server, req)
}

// remoteRoomIDFor resolves an alias that belongs to another server.
//
// Only the alias's own server is asked. A server is entitled to move its aliases, and a third server
// reporting what `#room:elsewhere` means is repeating hearsay; if the resident server refuses, the
// user learns that from the error rather than joining a room somebody else remembers.
func (s *Service) remoteRoomIDFor(ctx context.Context, alias string) (string, error) {
	if s.remoteAliases == nil {
		_, host, _ := matrix.ParseAlias(alias)
		return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"Alias %q belongs to %s, which this server cannot ask: outbound federation is off", alias, host)
	}
	return s.remoteAliases.LookupAlias(ctx, alias)
}

// isLocalServer reports whether a server name names this homeserver.
//
// Comparison is case-insensitive because a client is allowed to type the host in any case it likes
// and a Matrix server name is a DNS name.
func (s *Service) isLocalServer(server string) bool {
	return server == "" || strings.EqualFold(server, s.cfg.ServerName)
}
