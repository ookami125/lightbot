package main

import "log"

func logError(e error) {
	log.Printf("ERROR: %q\n", e)
}

func logWarning(e error) {
	log.Printf("WARNING: %q\n", e)
}
