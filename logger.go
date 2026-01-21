package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

type logLevel int32

const (
	logDebug logLevel = iota
	logInfo
	logWarn
	logError
	logOff
)

var currentLogLevel atomic.Int32

func init() {
	currentLogLevel.Store(int32(logInfo))
}

func parseLogLevel(raw string) (logLevel, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug", "dbg":
		return logDebug, nil
	case "info", "":
		return logInfo, nil
	case "warn", "warning":
		return logWarn, nil
	case "error", "err":
		return logError, nil
	case "off", "none", "silent":
		return logOff, nil
	default:
		return logInfo, fmt.Errorf("unknown log level: %q", raw)
	}
}

func setLogLevel(level logLevel) {
	currentLogLevel.Store(int32(level))
}

func logEnabled(level logLevel) bool {
	return logLevel(currentLogLevel.Load()) <= level
}

func logDebugf(format string, args ...any) {
	if logEnabled(logDebug) {
		log.Printf(format, args...)
	}
}

func logInfof(format string, args ...any) {
	if logEnabled(logInfo) {
		log.Printf(format, args...)
	}
}

func logWarnf(format string, args ...any) {
	if logEnabled(logWarn) {
		log.Printf(format, args...)
	}
}

func logErrorf(format string, args ...any) {
	if logEnabled(logError) {
		log.Printf(format, args...)
	}
}

func fatalf(format string, args ...any) {
	if logEnabled(logError) {
		log.Printf(format, args...)
	}
	os.Exit(1)
}
