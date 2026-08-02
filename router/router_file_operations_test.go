package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
)

func TestDeleteServerFileOperationResponses(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	started := make(chan struct{})
	operation, done, err := s.FileOperations().Start(context.Background(), "copy_many", func(ctx context.Context, _ *server.FileOperation) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	require.NoError(t, err)
	<-started

	request := func(identifier string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/servers/"+s.ID()+"/files/operations/"+identifier, nil)
		req.Header.Set("Authorization", "Bearer "+config.Get().Token.Token)
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	require.Equal(t, http.StatusUnprocessableEntity, request("not-a-uuid").Code)
	require.Equal(t, http.StatusNotFound, request(uuid.NewString()).Code)
	require.Equal(t, http.StatusNoContent, request(operation.Identifier()).Code)
	require.ErrorIs(t, <-done, server.ErrFileOperationCanceled)
	require.Equal(t, http.StatusNotFound, request(operation.Identifier()).Code)
}
