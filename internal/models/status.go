package models

type Status int8

const (
	Active Status = iota - 1
	InProgress
	Closed
	Deleted
)