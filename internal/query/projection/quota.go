package projection

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zitadel/zitadel/internal/database"
	"github.com/zitadel/zitadel/internal/eventstore"
	"github.com/zitadel/zitadel/internal/eventstore/handler"
	"github.com/zitadel/zitadel/internal/eventstore/handler/crdb"
	"github.com/zitadel/zitadel/internal/repository/instance"
	"github.com/zitadel/zitadel/internal/repository/quota"
)

const (
	QuotasProjectionTable       = "projections.quotas"
	QuotaPeriodsProjectionTable = QuotasProjectionTable + "_" + quotaPeriodsTableSuffix
	QuotaNotificationsTable     = QuotasProjectionTable + "_" + quotaNotificationsTableSuffix

	QuotaColumnID         = "id"
	QuotaColumnInstanceID = "instance_id"
	QuotaColumnUnit       = "unit"
	QuotaColumnAmount     = "amount"
	QuotaColumnFrom       = "from_anchor"
	QuotaColumnInterval   = "interval"
	QuotaColumnLimit      = "limit_usage"

	quotaPeriodsTableSuffix     = "periods"
	QuotaPeriodColumnInstanceID = "instance_id"
	QuotaPeriodColumnUnit       = "unit"
	QuotaPeriodColumnStart      = "start"
	QuotaPeriodColumnUsage      = "usage"

	quotaNotificationsTableSuffix               = "notifications"
	QuotaNotificationColumnInstanceID           = "instance_id"
	QuotaNotificationColumnUnit                 = "unit"
	QuotaNotificationColumnID                   = "id"
	QuotaNotificationColumnCallURL              = "call_url"
	QuotaNotificationColumnPercent              = "percent"
	QuotaNotificationColumnRepeat               = "repeat"
	QuotaNotificationColumnLatestDuePeriodStart = "latest_due_period_start"
	QuotaNotificationColumnNextDueThreshold     = "next_due_threshold"
)

type quotaProjection struct {
	crdb.StatementHandler
	client *database.DB
}

func newQuotaProjection(ctx context.Context, config crdb.StatementHandlerConfig) *quotaProjection {
	p := new(quotaProjection)
	config.ProjectionName = QuotasProjectionTable
	config.Reducers = p.reducers()
	config.InitCheck = crdb.NewMultiTableCheck(
		crdb.NewTable(
			[]*crdb.Column{
				crdb.NewColumn(QuotaColumnID, crdb.ColumnTypeText),
				crdb.NewColumn(QuotaColumnInstanceID, crdb.ColumnTypeText),
				crdb.NewColumn(QuotaColumnUnit, crdb.ColumnTypeEnum),
				crdb.NewColumn(QuotaColumnAmount, crdb.ColumnTypeInt64),
				crdb.NewColumn(QuotaColumnFrom, crdb.ColumnTypeTimestamp),
				crdb.NewColumn(QuotaColumnInterval, crdb.ColumnTypeInterval),
				crdb.NewColumn(QuotaColumnLimit, crdb.ColumnTypeBool),
			},
			crdb.NewPrimaryKey(QuotaColumnInstanceID, QuotaColumnUnit),
		),
		crdb.NewSuffixedTable(
			[]*crdb.Column{
				crdb.NewColumn(QuotaPeriodColumnInstanceID, crdb.ColumnTypeText),
				crdb.NewColumn(QuotaPeriodColumnUnit, crdb.ColumnTypeEnum),
				crdb.NewColumn(QuotaPeriodColumnStart, crdb.ColumnTypeTimestamp),
				crdb.NewColumn(QuotaPeriodColumnUsage, crdb.ColumnTypeInt64),
			},
			crdb.NewPrimaryKey(QuotaPeriodColumnInstanceID, QuotaPeriodColumnUnit, QuotaPeriodColumnStart),
			quotaPeriodsTableSuffix,
		),
		crdb.NewSuffixedTable(
			[]*crdb.Column{
				crdb.NewColumn(QuotaNotificationColumnInstanceID, crdb.ColumnTypeText),
				crdb.NewColumn(QuotaNotificationColumnUnit, crdb.ColumnTypeEnum),
				crdb.NewColumn(QuotaNotificationColumnID, crdb.ColumnTypeText),
				crdb.NewColumn(QuotaNotificationColumnCallURL, crdb.ColumnTypeText),
				crdb.NewColumn(QuotaNotificationColumnPercent, crdb.ColumnTypeInt64),
				crdb.NewColumn(QuotaNotificationColumnRepeat, crdb.ColumnTypeBool),
				crdb.NewColumn(QuotaNotificationColumnLatestDuePeriodStart, crdb.ColumnTypeTimestamp, crdb.Nullable()),
				crdb.NewColumn(QuotaNotificationColumnNextDueThreshold, crdb.ColumnTypeInt64, crdb.Nullable()),
			},
			crdb.NewPrimaryKey(QuotaNotificationColumnInstanceID, QuotaNotificationColumnUnit, QuotaNotificationColumnID),
			quotaNotificationsTableSuffix,
		),
	)
	p.StatementHandler = crdb.NewStatementHandler(ctx, config)
	p.client = config.Client
	return p
}

func (q *quotaProjection) reducers() []handler.AggregateReducer {
	return []handler.AggregateReducer{
		{
			Aggregate: instance.AggregateType,
			EventRedusers: []handler.EventReducer{
				{
					Event:  instance.InstanceRemovedEventType,
					Reduce: q.reduceInstanceRemoved,
				},
			},
		},
		{
			Aggregate: quota.AggregateType,
			EventRedusers: []handler.EventReducer{
				{
					Event:  quota.AddedEventType,
					Reduce: q.reduceQuotaAdded,
				},
			},
		},
		{
			Aggregate: quota.AggregateType,
			EventRedusers: []handler.EventReducer{
				{
					Event:  quota.RemovedEventType,
					Reduce: q.reduceQuotaRemoved,
				},
			},
		},
		{
			Aggregate: quota.AggregateType,
			EventRedusers: []handler.EventReducer{
				{
					Event:  quota.NotificationDueEventType,
					Reduce: q.reduceQuotaNotificationDue,
				},
			},
		},
		{
			Aggregate: quota.AggregateType,
			EventRedusers: []handler.EventReducer{
				{
					Event:  quota.NotifiedEventType,
					Reduce: q.reduceQuotaNotified,
				},
			},
		},
	}
}

func (q *quotaProjection) reduceQuotaNotified(event eventstore.Event) (*handler.Statement, error) {
	return crdb.NewNoOpStatement(event), nil
}

func (q *quotaProjection) reduceQuotaAdded(event eventstore.Event) (*handler.Statement, error) {
	e, err := assertEvent[*quota.AddedEvent](event)
	if err != nil {
		return nil, err
	}

	createStatements := make([]func(e eventstore.Event) crdb.Exec, len(e.Notifications)+1)
	createStatements[0] = crdb.AddCreateStatement(
		[]handler.Column{
			handler.NewCol(QuotaColumnID, e.Aggregate().ID),
			handler.NewCol(QuotaColumnInstanceID, e.Aggregate().InstanceID),
			handler.NewCol(QuotaColumnUnit, e.Unit),
			handler.NewCol(QuotaColumnAmount, e.Amount),
			handler.NewCol(QuotaColumnFrom, e.From),
			handler.NewCol(QuotaColumnInterval, e.ResetInterval),
			handler.NewCol(QuotaColumnLimit, e.Limit),
		})
	for i := range e.Notifications {
		notification := e.Notifications[i]
		createStatements[i+1] = crdb.AddCreateStatement(
			[]handler.Column{
				handler.NewCol(QuotaNotificationColumnInstanceID, e.Aggregate().InstanceID),
				handler.NewCol(QuotaNotificationColumnUnit, e.Unit),
				handler.NewCol(QuotaNotificationColumnID, notification.ID),
				handler.NewCol(QuotaNotificationColumnCallURL, notification.CallURL),
				handler.NewCol(QuotaNotificationColumnPercent, notification.Percent),
				handler.NewCol(QuotaNotificationColumnRepeat, notification.Repeat),
			},
			crdb.WithTableSuffix(quotaNotificationsTableSuffix),
		)
	}

	return crdb.NewMultiStatement(e, createStatements...), nil
}

func (q *quotaProjection) reduceQuotaNotificationDue(event eventstore.Event) (*handler.Statement, error) {
	e, err := assertEvent[*quota.NotificationDueEvent](event)
	if err != nil {
		return nil, err
	}
	return crdb.NewUpdateStatement(e,
		[]handler.Column{
			handler.NewCol(QuotaNotificationColumnLatestDuePeriodStart, e.PeriodStart),
			{
				Name:  QuotaNotificationColumnNextDueThreshold,
				Value: e.Threshold,
				ParameterOpt: func(thresholdFromEvent string) string {
					// We increment the threshold if the periodStart matches, else we reset it to percent
					// TODO: Use multiple parameters for a single column
					return fmt.Sprintf("CASE WHEN %s = '%s' THEN CAST ( floor ( %s / %s + 1 ) AS INT ) * %s ELSE %s END", QuotaNotificationColumnLatestDuePeriodStart, e.PeriodStart.Format(time.RFC3339), thresholdFromEvent, QuotaNotificationColumnPercent, QuotaNotificationColumnPercent, QuotaNotificationColumnPercent)
				},
			},
		},
		[]handler.Condition{
			handler.NewCond(QuotaNotificationColumnInstanceID, e.Aggregate().InstanceID),
			handler.NewCond(QuotaNotificationColumnUnit, e.Unit),
			handler.NewCond(QuotaNotificationColumnID, e.ID),
		},
		crdb.WithTableSuffix(quotaNotificationsTableSuffix),
	), nil
}

func (q *quotaProjection) reduceQuotaRemoved(event eventstore.Event) (*handler.Statement, error) {
	e, err := assertEvent[*quota.RemovedEvent](event)
	if err != nil {
		return nil, err
	}
	return crdb.NewMultiStatement(
		e,
		crdb.AddDeleteStatement(
			[]handler.Condition{
				handler.NewCond(QuotaPeriodColumnInstanceID, e.Aggregate().InstanceID),
				handler.NewCond(QuotaPeriodColumnUnit, e.Unit),
			},
			crdb.WithTableSuffix(quotaPeriodsTableSuffix),
		),
		crdb.AddDeleteStatement(
			[]handler.Condition{
				handler.NewCond(QuotaNotificationColumnInstanceID, e.Aggregate().InstanceID),
				handler.NewCond(QuotaNotificationColumnUnit, e.Unit),
			},
			crdb.WithTableSuffix(quotaNotificationsTableSuffix),
		),
		crdb.AddDeleteStatement(
			[]handler.Condition{
				handler.NewCond(QuotaColumnInstanceID, e.Aggregate().InstanceID),
				handler.NewCond(QuotaColumnUnit, e.Unit),
			},
		),
	), nil
}

func (q *quotaProjection) reduceInstanceRemoved(event eventstore.Event) (*handler.Statement, error) {
	// we only assert the event to make sure it is the correct type
	e, err := assertEvent[*instance.InstanceRemovedEvent](event)
	if err != nil {
		return nil, err
	}
	return crdb.NewMultiStatement(
		e,
		crdb.AddDeleteStatement(
			[]handler.Condition{
				handler.NewCond(QuotaPeriodColumnInstanceID, e.Aggregate().InstanceID),
			},
			crdb.WithTableSuffix(quotaPeriodsTableSuffix),
		),
		crdb.AddDeleteStatement(
			[]handler.Condition{
				handler.NewCond(QuotaNotificationColumnInstanceID, e.Aggregate().InstanceID),
			},
			crdb.WithTableSuffix(quotaNotificationsTableSuffix),
		),
		crdb.AddDeleteStatement(
			[]handler.Condition{
				handler.NewCond(QuotaColumnInstanceID, e.Aggregate().InstanceID),
			},
		),
	), nil
}

func (q *quotaProjection) IncrementUsage(ctx context.Context, unit quota.Unit, instanceID string, periodStart time.Time, count uint64) error {
	if count == 0 {
		return nil
	}
	insertCols := []string{QuotaPeriodColumnInstanceID, QuotaPeriodColumnUnit, QuotaPeriodColumnStart, QuotaPeriodColumnUsage}
	conflictTarget := []string{QuotaPeriodColumnInstanceID, QuotaPeriodColumnUnit, QuotaPeriodColumnStart}
	vals := []interface{}{instanceID, unit, periodStart, count, count}
	params := make([]string, len(vals))
	for i := range vals {
		params[i] = "$" + strconv.Itoa(i+1)
	}
	_, err := q.client.ExecContext(
		ctx,
		fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s = %s.%s + %s",
			QuotaPeriodsProjectionTable,
			strings.Join(insertCols, ", "),
			strings.Join(params[0:len(params)-1], ", "),
			strings.Join(conflictTarget, ", "),
			QuotaPeriodColumnUsage,
			QuotaPeriodsProjectionTable,
			QuotaPeriodColumnUsage,
			params[len(params)-1]),
		vals...,
	)
	return err
}