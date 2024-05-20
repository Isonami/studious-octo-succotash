FROM node as frontend

COPY frontend .
RUN npm install
RUN npm run build

FROM golang as builder

WORKDIR /go/src/app
COPY . .
COPY --from=frontend build ./frontend/

ENV SKIP_NPM=1
ENV GOOS=linux
ENV GOARCH=arm64


RUN go mod download
RUN go generate
RUN go vet -v

RUN CGO_ENABLED=0 go build -o /go/bin/studious-octo-succotash

FROM --platform=arm64 ubuntu

COPY --from=builder /go/bin/studious-octo-succotash /usr/bin

RUN apt update && apt install -y openssh-client rsync && apt clean && rm -rf /var/lib/apt/lists/*

CMD ["/usr/bin/studious-octo-succotash"]
