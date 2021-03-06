package command

import (
	"bytes"
	"strings"
	"time"

	"github.com/meitu/titan/db"
	"github.com/meitu/titan/encoding/resp"
	"github.com/meitu/titan/metrics"
	"go.uber.org/zap"
)

// Multi starts a transaction which will block subsequent commands until 'exec'
func Multi(ctx *Context) {
	ctx.Client.Multi = true
	resp.ReplySimpleString(ctx.Out, OK)
}

// Exec all the commands queued in client
func Exec(ctx *Context) {
	ctx.Client.Multi = false
	commands := ctx.Client.Commands
	if len(commands) == 0 {
		resp.ReplyArray(ctx.Out, 0)
		return
	}

	txn := ctx.Client.Txn
	// txn has not begun (watch is not called )
	var err error
	if txn == nil {
		txn, err = ctx.Client.DB.Begin()
		if err != nil {
			resp.ReplyError(ctx.Out, "ERR "+err.Error())
			return
		}
	}

	size := len(commands)
	outputs := make([]*bytes.Buffer, size)
	onCommits := make([]OnCommit, size)
	for i, cmd := range commands {
		var onCommit OnCommit
		out := bytes.NewBuffer(nil)
		subCtx := &Context{
			Name:    cmd.Name,
			Args:    cmd.Args,
			In:      ctx.In,
			Out:     out,
			Context: ctx.Context,
		}
		name := strings.ToLower(cmd.Name)
		if _, ok := txnCommands[name]; ok {
			onCommit, err = TxnCall(subCtx, txn)
			if err != nil {
				resp.ReplyError(out, err.Error())
			}
		} else {
			Call(subCtx)
		}
		onCommits[i] = onCommit
		outputs[i] = out
	}
	start := time.Now()
	mt := metrics.GetMetrics()
	defer func() {
		cost := time.Since(start).Seconds()
		mt.TxnCommitHistogramVec.WithLabelValues(ctx.Client.Namespace, ctx.Name).Observe(cost)
	}()
	err = txn.Commit(ctx)
	if err != nil {
		if db.IsConflictError(err) {
			mt.TxnConflictsCounterVec.WithLabelValues(ctx.Client.Namespace, ctx.Name).Inc()
		}
		mt.TxnFailuresCounterVec.WithLabelValues(ctx.Client.Namespace, ctx.Name).Inc()
		zap.L().Error("commit failed",
			zap.Int64("clientid", ctx.Client.ID),
			zap.String("command", ctx.Name),
			zap.String("traceid", ctx.TraceID),
			zap.Error(err))
		resp.ReplyArray(ctx.Out, 0)
		return
	}

	ctx.Client.Commands = nil

	resp.ReplyArray(ctx.Out, size)
	// run OnCommit that fill reply to outputs
	for i := range onCommits {
		c := onCommits[i]
		if c != nil {
			c()
		}

		if _, err := ctx.Out.Write(outputs[i].Bytes()); err != nil {
			zap.L().Error("reply to client failed",
				zap.Int64("clientid", ctx.Client.ID),
				zap.String("command", ctx.Name),
				zap.String("traceid", ctx.TraceID),
				zap.Error(err))
			break
		}
	}
}

// Watch starts a transaction, watch is a global transaction and is not key associated(this is different from redis)
func Watch(ctx *Context) {
	txn, err := ctx.Client.DB.Begin()
	if err != nil {
		resp.ReplyError(ctx.Out, "Err "+err.Error())
		return
	}
	keys := make([][]byte, len(ctx.Args))
	for i := range ctx.Args {
		keys[i] = []byte(ctx.Args[i])
	}
	if err := txn.LockKeys(keys...); err != nil {
		txn.Rollback()
		resp.ReplyError(ctx.Out, "Err "+err.Error())
		return
	}
	ctx.Client.Txn = txn
	resp.ReplySimpleString(ctx.Out, OK)
}

// Discard flushes all previously queued commands in a transaction and restores the connection state to normal
func Discard(ctx *Context) {
	// in watch state, the txn has begun, rollback it
	if ctx.Client.Txn != nil {
		ctx.Client.Txn.Rollback()
		ctx.Client.Txn = nil
	}
	ctx.Client.Commands = nil
	ctx.Client.Multi = false
	resp.ReplySimpleString(ctx.Out, OK)
}

// Unwatch flushes all the previously watched keys for a transaction
func Unwatch(ctx *Context) {
	if ctx.Client.Txn != nil {
		ctx.Client.Txn.Rollback()
		ctx.Client.Txn = nil
	}
	resp.ReplySimpleString(ctx.Out, OK)
}
