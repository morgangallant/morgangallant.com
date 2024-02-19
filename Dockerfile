FROM golang:bullseye AS build
WORKDIR /mg
ADD . .
RUN GOOS=linux GOARCH=amd64 go build -o website main.go

FROM golang:bullseye
COPY --from=build /mg/website /usr/bin/program
ENTRYPOINT ["/usr/bin/program"]
