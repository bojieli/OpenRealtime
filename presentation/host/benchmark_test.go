package host

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkRouterDispatch64(b *testing.B) {
	router := newRouter()
	routes := make([]Route, 0, 64)
	for index := 0; index < 64; index++ {
		routes = append(routes, Route{
			Pattern: fmt.Sprintf("GET /route/%d", index),
			Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusNoContent)
			}),
		})
	}
	if _, err := router.Register("benchmark", routes); err != nil {
		b.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/route/63", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			b.Fatal("unexpected response")
		}
	}
}
