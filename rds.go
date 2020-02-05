package main

import (
	"sync"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

// DBMaxConnections is a hardcoded map of instance types and DB Parameter Group names
// This is a dump workaround created because by default the DB Parameter Group `max_connections` is a function
// that is hard to parse and process in code and it contains a variable whose value is unknown to us (DBInstanceClassMemory)
// AWS has no means to return the actual `max_connections` value.
var DBMaxConnections = map[string]map[string]int64{
	"db.t2.small": map[string]int64{
		"default": 150,
		"default.mysql5.7": 150,
	},
	"db.m5.2xlarge": map[string]int64{
		"default": 3429,
		"default.postgres10": 3429,
		"default.postgres11": 3429,
	},
	"db.m5.large": map[string]int64{
		"default": 823,
		"default.postgres10": 823,
		"default.postgres11": 823,
	},
}

// RDSExporter defines an instance of the RDS Exporter
type RDSExporter struct {
	sess                       *session.Session
	AllocatedStorage           *prometheus.Desc
	DBInstanceClass            *prometheus.Desc
	DBInstanceStatus           *prometheus.Desc
	EngineVersion              *prometheus.Desc
	LatestRestorableTime       *prometheus.Desc
	MaxConnections             *prometheus.Desc
	MaxConnectionsMappingError *prometheus.Desc
	PubliclyAccessible         *prometheus.Desc
	StorageEncrypted           *prometheus.Desc

	mutex *sync.Mutex
}

// NewRDSExporter creates a new RDSExporter instance
func NewRDSExporter(sess *session.Session) *RDSExporter {
	log.Info("[RDS] Initializing RDS exporter")
	return &RDSExporter{
		sess:  sess,
		mutex: &sync.Mutex{},
		AllocatedStorage: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_allocatedstorage"),
			"The amount of allocated storage in bytes.",
			[]string{"aws_region", "dbinstance_identifier"},
			nil,
		),
		DBInstanceClass: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_dbinstanceclass"),
			"The DB instance class (type).",
			[]string{"aws_region", "dbinstance_identifier", "instance_class"},
			nil,
		),
		DBInstanceStatus: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_dbinstancestatus"),
			"The instance status.",
			[]string{"aws_region", "dbinstance_identifier", "instance_status"},
			nil,
		),
		EngineVersion: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_engineversion"),
			"The DB engine type and version.",
			[]string{"aws_region", "dbinstance_identifier", "engine", "engine_version"},
			nil,
		),
		LatestRestorableTime: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_latestrestorabletime"),
			"Latest restorable time (UTC date timestamp).",
			[]string{"aws_region", "dbinstance_identifier"},
			nil,
		),
		MaxConnections: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_maxconnections"),
			"The DB's max_connections value",
			[]string{"aws_region", "dbinstance_identifier"},
			nil,
		),
		MaxConnectionsMappingError: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_maxconnections_error"),
			"Indicates no mapping found for instance/parameter group.",
			[]string{"aws_region", "dbinstance_identifier", "instance_class"},
			nil,
		),
		PubliclyAccessible: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_publiclyaccessible"),
			"Indicates if the DB is publicly accessible",
			[]string{"aws_region", "dbinstance_identifier"},
			nil,
		),
		StorageEncrypted: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "rds_storageencrypted"),
			"Indicates if the DB storage is encrypted",
			[]string{"aws_region", "dbinstance_identifier"},
			nil,
		),
	}
}

// Describe is used by the Prometheus client to return a description of the metrics
func (e *RDSExporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.AllocatedStorage
	ch <- e.DBInstanceClass
	ch <- e.DBInstanceStatus
	ch <- e.EngineVersion
	ch <- e.LatestRestorableTime
	ch <- e.MaxConnections
	ch <- e.MaxConnectionsMappingError
	ch <- e.PubliclyAccessible
	ch <- e.StorageEncrypted
}

// Collect is used by the Prometheus client to collect and return the metrics values
func (e *RDSExporter) Collect(ch chan<- prometheus.Metric) {
	svc := rds.New(e.sess)
	input := &rds.DescribeDBInstancesInput{}

	// Get all DB instances.
	// If a Marker is found, do pagination until last page
	var instances []*rds.DBInstance
	for {
		exporterMetrics.IncrementRequests()
		result, err := svc.DescribeDBInstances(input)
		if err != nil {
			log.Errorf("[RDS] Call to DescribeDBInstances failed in region %s: %s", *e.sess.Config.Region, err)
			exporterMetrics.IncrementErrors()
			return
		}
		instances = append(instances, result.DBInstances...)
		input.Marker = result.Marker
		if result.Marker == nil {
			break
		}
	}

	for _, instance := range instances {
		var maxConnections int64
		if val, ok := DBMaxConnections[*instance.DBInstanceClass]; ok {
			if val, ok := val[*instance.DBParameterGroups[0].DBParameterGroupName]; ok {
				log.Debugf("[RDS] Found mapping for instance type %s group %s value %d",
					*instance.DBInstanceClass,
					*instance.DBParameterGroups[0].DBParameterGroupName,
					val)
				maxConnections = val
				ch <- prometheus.MustNewConstMetric(e.MaxConnectionsMappingError, prometheus.GaugeValue, 0, *e.sess.Config.Region, *instance.DBInstanceIdentifier, *instance.DBInstanceClass)
			} else {
				log.Errorf("[RDS] No DB max_connections mapping exists for instance type %s parameter group %s",
					*instance.DBInstanceClass,
					*instance.DBParameterGroups[0].DBParameterGroupName)
				ch <- prometheus.MustNewConstMetric(e.MaxConnectionsMappingError, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier, *instance.DBInstanceClass)
			}
		} else {
			log.Errorf("[RDS] No DB max_connections mapping exists for instance type %s",
				*instance.DBInstanceClass)
			ch <- prometheus.MustNewConstMetric(e.MaxConnectionsMappingError, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier, *instance.DBInstanceClass)
		}

		if *instance.PubliclyAccessible {
			ch <- prometheus.MustNewConstMetric(e.PubliclyAccessible, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier)

		} else {
			ch <- prometheus.MustNewConstMetric(e.PubliclyAccessible, prometheus.GaugeValue, 0, *e.sess.Config.Region, *instance.DBInstanceIdentifier)

		}

		if *instance.StorageEncrypted {
			ch <- prometheus.MustNewConstMetric(e.StorageEncrypted, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier)

		} else {
			ch <- prometheus.MustNewConstMetric(e.StorageEncrypted, prometheus.GaugeValue, 0, *e.sess.Config.Region, *instance.DBInstanceIdentifier)

		}

		ch <- prometheus.MustNewConstMetric(e.MaxConnections, prometheus.GaugeValue, float64(maxConnections), *e.sess.Config.Region, *instance.DBInstanceIdentifier)
		ch <- prometheus.MustNewConstMetric(e.AllocatedStorage, prometheus.GaugeValue, float64(*instance.AllocatedStorage*1024*1024*1024), *e.sess.Config.Region, *instance.DBInstanceIdentifier)
		ch <- prometheus.MustNewConstMetric(e.DBInstanceStatus, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier, *instance.DBInstanceStatus)
		ch <- prometheus.MustNewConstMetric(e.EngineVersion, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier, *instance.Engine, *instance.EngineVersion)
		ch <- prometheus.MustNewConstMetric(e.DBInstanceClass, prometheus.GaugeValue, 1, *e.sess.Config.Region, *instance.DBInstanceIdentifier, *instance.DBInstanceClass)
		ch <- prometheus.MustNewConstMetric(e.LatestRestorableTime, prometheus.CounterValue, float64(instance.LatestRestorableTime.Unix()), *e.sess.Config.Region, *instance.DBInstanceIdentifier)
	}
}
