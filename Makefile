.PHONY: run test demo

run:
	go run .

test:
	go test -v ./...

# Start the server in the background, hit a few routes, then stop it.
demo:
	@go build -o /tmp/chi-play-demo . && (/tmp/chi-play-demo >/dev/null 2>&1 & echo $$! > /tmp/chi-play-demo.pid); sleep 1
	@for c in "/" "/hello/gopher" "/square/7" "/files/a/b.txt" "/greet?name=bill" "/todos" "/todos/1" "/v2/ping" "/nope"; do \
		printf '\n$$ curl localhost:3000%s\n' "$$c"; curl -s localhost:3000$$c; done
	@printf '\n$$ curl -X POST /todos\n'; curl -s -X POST localhost:3000/todos -d '{"title":"learn chi"}'
	@printf '\n$$ curl /admin/stats (no key)\n'; curl -s localhost:3000/admin/stats
	@printf '\n$$ curl /admin/stats (with key)\n'; curl -s -H 'X-API-Key: secret' localhost:3000/admin/stats
	@kill `cat /tmp/chi-play-demo.pid`; rm -f /tmp/chi-play-demo.pid /tmp/chi-play-demo
