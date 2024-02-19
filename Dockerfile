FROM golang:bullseye AS build
WORKDIR /mg
ADD . .
RUN GOOS=linux GOARCH=amd64 go build -o website main.go

FROM gcr.io/distroless/static-debian12
COPY --from=build /mg/website /website
ENTRYPOINT ["/website"]
