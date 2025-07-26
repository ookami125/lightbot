FROM golang:1.19

WORKDIR /usr/src/app
RUN mkdir -p /usr/src/app/data

# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN go build -v -o /usr/local/bin/app ./src/...

RUN chmod +x ./run.sh


CMD ["/bin/bash", "-c", "./run.sh"]