package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"gopkg.in/matryer/try.v1"
)

var (
	port                  = flag.Int("p", 8080, "Port to bind at")
	initialConnectTimeout = 1 * time.Minute
	reconnectionTimeout   = 10 * time.Second
)

func main() {
	flag.Parse()
	log.Println("Pinging Docker...")

	cli, err := client.NewEnvClient()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err = cli.Ping(ctx)
	}

	if err != nil {
		panic(err)
	}

	log.Println("Docker daemon is available!")

	deathNote := sync.Map{}

	connectionAccepted := make(chan net.Addr)
	connectionLost := make(chan net.Addr)

	go processRequests(&deathNote, connectionAccepted, connectionLost)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	waitForPruneCondition(ctx, connectionAccepted, connectionLost)

	prune(cli, &deathNote)
}

func processRequests(deathNote *sync.Map, connectionAccepted chan<- net.Addr, connectionLost chan<- net.Addr) {
	log.Printf("Starting on port %d...", *port)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))

	if err != nil {
		panic(err)
	}
	log.Println("Started!")
	for {
		conn, err := ln.Accept()
		if err != nil {
			panic(err)
		}

		connectionAccepted <- conn.RemoteAddr()

		go func(conn net.Conn) {
			defer conn.Close()
			defer func() { connectionLost <- conn.RemoteAddr() }()

			reader := bufio.NewReader(conn)
			for {
				message, err := reader.ReadString('\n')

				message = strings.TrimSpace(message)

				if len(message) > 0 {
					query, err := url.ParseQuery(message)

					if err != nil {
						log.Println(err)
						continue
					}

					args := filters.NewArgs()
					for filterType, values := range query {
						for _, value := range values {
							args.Add(filterType, value)
						}
					}
					param, err := filters.ToParam(args)

					if err != nil {
						log.Println(err)
						continue
					}

					log.Printf("Adding %s", param)

					deathNote.Store(param, true)

					conn.Write([]byte("ACK\n"))
				}

				if err != nil {
					log.Println(err)
					break
				}
			}
		}(conn)
	}
}

func waitForPruneCondition(ctx context.Context, connectionAccepted <-chan net.Addr, connectionLost <-chan net.Addr) {
	connectionCount := 0
	never := make(chan time.Time, 1)
	defer close(never)

	handleConnectionAccepted := func(addr net.Addr) {
		log.Printf("New client connected: %s", addr)
		connectionCount++
	}

	select {
	case <-time.After(initialConnectTimeout):
		panic("Timed out waiting for the first connection")
	case addr := <-connectionAccepted:
		handleConnectionAccepted(addr)
	case <-ctx.Done():
		log.Println("Signal received")
		return
	}

	for {
		var noConnectionTimeout <-chan time.Time
		if connectionCount == 0 {
			noConnectionTimeout = time.After(reconnectionTimeout)
		} else {
			noConnectionTimeout = never
		}

		select {
		case addr := <-connectionAccepted:
			handleConnectionAccepted(addr)
			break
		case addr := <-connectionLost:
			log.Printf("Client disconnected: %s", addr.String())
			connectionCount--
			break
		case <-ctx.Done():
			log.Println("Signal received")
			return
		case <-noConnectionTimeout:
			log.Println("Timed out waiting for re-connection")
			return
		}
	}
}

func prune(cli *client.Client, deathNote *sync.Map) {
	deletedContainers := make(map[string]bool)
	deletedNetworks := make(map[string]bool)
	deletedVolumes := make(map[string]bool)
	deletedImages := make(map[string]bool)

	deathNote.Range(func(note, _ interface{}) bool {
		param := fmt.Sprint(note)
		log.Printf("Deleting %s\n", param)

		args, err := filters.FromParam(param)
		if err != nil {
			log.Println(err)
			return true
		}

		if containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{All: true, Filters: args}); err != nil {
			log.Println(err)
		} else {
			for _, container := range containers {
				cli.ContainerRemove(context.Background(), container.ID, types.ContainerRemoveOptions{RemoveVolumes: true, Force: true})
				deletedContainers[container.ID] = true
			}
		}

		try.Do(func(attempt int) (bool, error) {
			networksPruneReport, err := cli.NetworksPrune(context.Background(), args)
			for _, networkID := range networksPruneReport.NetworksDeleted {
				deletedNetworks[networkID] = true
			}
			shouldRetry := attempt < 10
			if err != nil && shouldRetry {
				log.Printf("Network pruning has failed, retrying(%d/%d). The error was: %v", attempt, 10, err)
				time.Sleep(1 * time.Second)
			}
			return shouldRetry, err
		})

		try.Do(func(attempt int) (bool, error) {
			volumesPruneReport, err := cli.VolumesPrune(context.Background(), args)
			for _, volumeName := range volumesPruneReport.VolumesDeleted {
				deletedVolumes[volumeName] = true
			}
			shouldRetry := attempt < 10
			if err != nil && shouldRetry {
				log.Printf("Volumes pruning has failed, retrying(%d/%d). The error was: %v", attempt, 10, err)
				time.Sleep(1 * time.Second)
			}
			return shouldRetry, err
		})

		try.Do(func(attempt int) (bool, error) {
			args.Add("dangling", "false")
			imagesPruneReport, err := cli.ImagesPrune(context.Background(), args)
			for _, image := range imagesPruneReport.ImagesDeleted {
				deletedImages[image.Deleted] = true
			}
			shouldRetry := attempt < 10
			if err != nil && shouldRetry {
				log.Printf("Images pruning has failed, retrying(%d/%d). The error was: %v", attempt, 10, err)
				time.Sleep(1 * time.Second)
			}
			return shouldRetry, err
		})

		return true
	})

	log.Printf("Removed %d container(s), %d network(s), %d volume(s) %d image(s)", len(deletedContainers), len(deletedNetworks), len(deletedVolumes), len(deletedImages))
}
