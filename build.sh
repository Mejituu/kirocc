go build -ldflags="-s -w" -o dist/kirocc ./cmd/kirocc
cp -fv dist/kirocc ~/.local/bin/kirocc
