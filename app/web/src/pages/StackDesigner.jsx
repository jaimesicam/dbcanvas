import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { stackApi, frameApi, TTL_OPTIONS, DEPLOY_TONE, NODE_UPLOAD_DESTS, PRODUCT_OS_FAMILIES } from '../lib/stackApi.js'
import { kindOf as aioKindOf, familyOf as aioFamilyOf } from '../lib/aioPorts.js'
import IntranetManager from './IntranetManager.jsx'
import SambaManager from './SambaManager.jsx'
import PMMManager from './PMMManager.jsx'
import PXCManager from './PXCManager.jsx'
import ProxySQLManager from './ProxySQLManager.jsx'
import MySQLManager from './MySQLManager.jsx'
import InnoDBManager from './InnoDBManager.jsx'
import MongoDBManager from './MongoDBManager.jsx'
import SeaweedFSManager from './SeaweedFSManager.jsx'
import OpenBaoManager from './OpenBaoManager.jsx'
import K3DManager from './K3DManager.jsx'
import PatroniManager from './PatroniManager.jsx'
import HAProxyManager from './HAProxyManager.jsx'
import PGManager from './PGManager.jsx'
import RepmgrManager from './RepmgrManager.jsx'
import SpockManager from './SpockManager.jsx'
import { AllInOneForm, AllInOneManager } from './AllInOne.jsx'
import {
  MariaDBNodeForm, MariaDBFrameForm, MariaDBGaleraFrameForm,
  MySQLCENodeForm, MySQLCEFrameForm, MySQLCEInnoDBFrameForm,
  UpstreamMemberForm,
} from './UpstreamForms.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import FileManager from './FileManager.jsx'
import { SecretInline, CopyButton as CopyBtn } from '../components/Secret.jsx'
import {
  PORTS, dist, portPoint, edgePath, screenToWorld, zoomAt,
} from '../lib/canvas.js'
import { useSettings } from '../settings/SettingsProvider.jsx'

const NODE_W = 212
const NODE_H = 104
const SNAP = 26

// Node-type catalog.
export const NODE_TYPES = {
  intranet: {
    label: 'Intranet',
    sub: 'Squid Proxy · DNS · Mail · OpenLDAP · CA',
    color: '#6366f1',
    icon: 'Server',
    singleton: true,
    ports: false, // self-contained; no connection endpoints
    osOptions: [{ id: 'oel9', label: 'Oracle Linux 9' }],
  },
  sambaad: {
    label: 'Samba AD DC',
    slug: 'sambaad',
    sub: 'Active Directory · LDAP · Kerberos',
    color: '#0ea5e9',
    icon: 'Server',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'ubuntu', label: 'Ubuntu 24.04' }],
    defaults: { osVersion: '24.04', generateCert: false, certTtlValue: 365, certTtlUnit: 'days', useProxy: false },
  },
  pmm: {
    label: 'PMM3',
    slug: 'pmm',
    sub: 'Percona Monitoring & Management',
    color: '#0ea5e9',
    icon: 'Monitor',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'pmm', label: 'percona/pmm-server' }],
    defaults: { version: '', adminPassword: '', generateCert: false, watchtowerNodeId: '' },
  },
  // PXC nodes live inside a PXC cluster frame (not added from the toolbar
  // directly); this entry only supplies the color/icon used to render them.
  pxc: {
    label: 'PXC Node',
    slug: 'pxc',
    sub: 'Percona XtraDB Cluster',
    color: '#a855f7',
    icon: 'Database',
  },
  // Percona Server replication members live inside a Percona Server Replication frame.
  mysql: {
    label: 'Percona Server',
    slug: 'mysql',
    sub: 'Percona Server replication member',
    color: '#2563eb',
    icon: 'Database',
  },
  // InnoDB Cluster / GR members live inside an InnoDB Cluster/GR frame.
  innodb: {
    label: 'InnoDB Cluster / GR',
    slug: 'innodb',
    sub: 'Group Replication member',
    color: '#0891b2',
    icon: 'Database',
  },
  // PS MongoDB members (mongod shard/config + mongos router) live inside a fixed
  // PSMDB Sharded Cluster frame.
  psmdb: {
    label: 'PS MongoDB',
    slug: 'psmdb',
    sub: 'PS MongoDB member',
    color: '#10b981',
    icon: 'Database',
  },
  // PS MongoDB replica-set members live inside a PSMDB RS frame.
  psmrs: {
    label: 'PSMDB RS',
    slug: 'psmrs',
    sub: 'PS MongoDB replica-set member',
    color: '#059669',
    icon: 'Database',
  },
  // Patroni members (PostgreSQL + Patroni + etcd) live inside a Patroni cluster frame.
  patroni: {
    label: 'Patroni',
    slug: 'patroni',
    sub: 'PostgreSQL + Patroni + etcd',
    color: '#336791',
    icon: 'Database',
  },
  // repmgr members (PostgreSQL + repmgr streaming replication) live inside a repmgr
  // cluster frame.
  repmgr: {
    label: 'repmgr',
    slug: 'repmgr',
    sub: 'PostgreSQL + repmgr',
    color: '#0e7490',
    icon: 'Database',
  },
  spock: {
    label: 'Spock',
    slug: 'spock',
    sub: 'PostgreSQL + Spock (multi-master)',
    color: '#dc2626',
    icon: 'Database',
  },
  // k3s nodes inside a K3D cluster frame (the first is the server, the rest agents).
  k3d: {
    label: 'k3s node',
    slug: 'k3s',
    sub: 'Kubernetes node (k3s)',
    color: '#326ce5',
    icon: 'Grid',
  },
  // Standalone single Percona Server for MongoDB instance (no replication).
  psm: {
    label: 'PSMDB',
    slug: 'psm',
    sub: 'PS MongoDB (standalone)',
    color: '#059669',
    icon: 'Database',
    singleton: false,
    ports: true, // connectable — a Hotel Sim node links to it
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', psmdbMajor: '8.0', psmdbVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
      enableOIDC: false, keycloakNodeId: '', oidcRealm: 'mongodb',
      oidcClientId: 'mongodb-client', oidcAuthClaim: 'MyClaim', oidcUseAuthClaim: true,
      enableVault: false, openbaoNodeId: '',
    },
  },
  // Standalone single Percona Server instance (no replication).
  ps: {
    label: 'Percona Server',
    slug: 'ps',
    sub: 'Percona Server (standalone)',
    color: '#2563eb',
    icon: 'Database',
    singleton: false,
    ports: true, // connectable — Airline Sim, MarketChaos and Stock Market Sim link to it
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', psMajor: '8.0', psVersion: '',
      rootPassword: '', gtid: true, pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
      enableOIDC: false, keycloakNodeId: '', oidcRealm: 'dbcanvas',
      enableVault: false, openbaoNodeId: '',
    },
  },
  // MariaDB, from mariadb.org. Both packaging families are offered: unlike the
  // Percona nodes this upstream publishes Debian builds dbcanvas can use directly.
  mariadb: {
    label: 'MariaDB',
    slug: 'mariadb',
    sub: 'MariaDB (standalone)',
    color: '#c0765a',
    icon: 'Database',
    singleton: false,
    ports: true, // connectable — a Stock Market Sim node links to it
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', mariadbMajor: '11.4', mariadbVersion: '',
      rootPassword: '', gtid: true, pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
    },
  },
  mariadbrepl: {
    label: 'MariaDB Replication',
    slug: 'mariadbrepl',
    sub: 'MariaDB replication member',
    color: '#c0765a',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: { exportEnabled: false, exportHostPort: 0 },
  },
  mariadbgalera: {
    label: 'MariaDB Galera',
    slug: 'mariadbgalera',
    sub: 'MariaDB Galera member',
    color: '#a85d43',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: { exportEnabled: false, exportHostPort: 0 },
  },
  // Oracle's MySQL Community builds, from repo.mysql.com.
  mysqlce: {
    label: 'MySQL',
    slug: 'mysqlce',
    sub: 'MySQL Community (standalone)',
    color: '#00758f',
    icon: 'Database',
    singleton: false,
    ports: true, // connectable — a Stock Market Sim node links to it
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', mysqlceMajor: '8.4', mysqlceVersion: '',
      rootPassword: '', gtid: true, pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
    },
  },
  mysqlcerepl: {
    label: 'MySQL Replication',
    slug: 'mysqlcerepl',
    sub: 'MySQL replication member',
    color: '#00758f',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: { exportEnabled: false, exportHostPort: 0 },
  },
  mysqlceinnodb: {
    label: 'MySQL InnoDB / GR',
    slug: 'mysqlceinnodb',
    sub: 'MySQL InnoDB Cluster member',
    color: '#005d72',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: { exportEnabled: false, exportHostPort: 0 },
  },
  // Standalone single PostgreSQL instance (no Patroni/etcd/replication).
  pg: {
    label: 'PostgreSQL',
    slug: 'pg',
    sub: 'PostgreSQL (standalone)',
    color: '#336791',
    icon: 'Database',
    singleton: false,
    ports: true, // connectable — a Car Rental Sim or Stock Market Sim node links to it
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', pgMajor: '16', pgVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      usePgBackRest: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
    },
  },
  proxysql: {
    label: 'ProxySQL',
    slug: 'proxysql',
    sub: 'ProxySQL — MySQL proxy',
    color: '#f59e0b',
    icon: 'ProxySQL',
    singleton: false,
    ports: true, // links to a PXC cluster frame (data flows PXC → ProxySQL)
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9',
      proxysqlMajor: '2', proxysqlVersion: '', mode: 'singlewrite',
      exportEnabled: false, exportHostPort: 0, pmmNodeId: '', useProxy: false,
    },
  },
  // HAProxy — a TCP load balancer fronting ONE database cluster: a Patroni PostgreSQL
  // cluster OR a Percona XtraDB Cluster (mutually exclusive). Links to the cluster frame
  // (data flows cluster → HAProxy) and routes writes/reads via the backend's health
  // checks (Patroni REST for Patroni; clustercheck :9200 for PXC).
  haproxy: {
    label: 'HAProxy',
    slug: 'haproxy',
    sub: 'HAProxy — PostgreSQL / PXC load balancer',
    color: '#22c55e',
    icon: 'ProxySQL',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9',
      exportEnabled: false, exportHostPort: 0, pmmNodeId: '', useProxy: false,
    },
  },
  // Percona Orchestrator — topology visualization / failure-detection for an async or
  // semi-sync MySQL replication frame (not PXC, and not the Galera/GR types: their
  // members elect their own primary). Unlike HAProxy/ProxySQL it is NOT wired via a
  // canvas association line: a replication frame optionally points at
  // it through its own "Monitored by (Orchestrator)" picker (orchestratorNodeId),
  // the same optional relationship PMM already has (pmmNodeId) — so it carries no
  // connection endpoints of its own. Its web UI is always published to the host
  // (like PMM and the app simulators), not an opt-in export toggle.
  orchestrator: {
    label: 'Orchestrator',
    slug: 'orchestrator',
    sub: 'Percona Orchestrator — MySQL topology & failure detection',
    color: '#f97316',
    icon: 'Monitor',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9',
      orchestratorVersion: '', alertEmail: 'admin', useProxy: false,
    },
  },
  // SeaweedFS — an S3-compatible object store (backup target). Like PMM it runs a
  // ready-made image (pulled at deploy), not a systemd OS image.
  seaweedfs: {
    label: 'SeaweedFS',
    slug: 'seaweedfs',
    sub: 'S3-compatible object storage (backups)',
    color: '#14b8a6',
    icon: 'Bucket',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'seaweedfs', label: 'chrislusf/seaweedfs' }],
    defaults: { accessKey: 'seaweedfs', secretKey: '', bucket: '' },
  },
  // Watchtower — a per-stack singleton running percona/watchtower with the docker
  // socket mounted and its HTTP API enabled. A PMM node associated with it can
  // trigger in-app server upgrades. Runs a ready-made image (pulled at deploy).
  watchtower: {
    label: 'Watchtower',
    slug: 'watchtower',
    sub: 'Container auto-upgrades (PMM)',
    color: '#475569',
    icon: 'Server',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'watchtower', label: 'percona/watchtower' }],
    defaults: {},
  },
  // Keycloak — a per-stack singleton OpenID Connect identity provider. A standalone
  // PSMDB node can enable MONGODB-OIDC authentication against it. Runs the upstream
  // keycloak image in dev mode (pulled at deploy); console published to the host.
  keycloak: {
    label: 'Keycloak',
    slug: 'keycloak',
    sub: 'OIDC identity provider',
    color: '#4f46e5',
    icon: 'Users',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'keycloak', label: 'quay.io/keycloak/keycloak' }],
    defaults: { generateCert: true, certTtlValue: 365, certTtlUnit: 'days' },
  },
  // OpenBao — a Vault-compatible secrets manager, used as the KMS for Percona data-at-rest
  // encryption (PS MySQL keyring_vault, PSMDB security.vault). Installed from EPEL on the
  // systemd Oracle Linux 9 image, so — unlike the pulled-image nodes — it is OEL9-only.
  // TLS from the Intranet CA is the default: the Percona engines verify it with that CA.
  openbao: {
    label: 'OpenBao',
    slug: 'openbao',
    sub: 'Secrets manager (Vault-compatible KMS)',
    color: '#eab308',
    icon: 'Vault',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux 9' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', useProxy: false,
      generateCert: true, certTtlValue: 365, certTtlUnit: 'days',
    },
  },
  // Ubuntu VNC — a desktop "jump box" (XFCE over web VNC) with the Percona client
  // tools preinstalled, for ad-hoc troubleshooting. Runs the pre-baked desktop image
  // dbcanvas-vnc:ubuntu-24.04-<arch> (`make vnc-image`), so the release is pinned and
  // the form asks for the architecture only.
  vnc: {
    label: 'Ubuntu VNC',
    slug: 'vnc',
    sub: 'Desktop + web VNC (DB clients)',
    color: '#dd4814',
    icon: 'Monitor',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'ubuntu', label: 'Ubuntu' }],
    defaults: {
      os: 'ubuntu', osVersion: '24.04',
      vncUser: 'dbadmin', vncPassword: '', useProxy: false,
    },
  },
  // Standalone Valkey — installed via percona-release (the "valkey-91" repo) on a
  // systemd base image, like every other Percona product. Analogue of the standalone
  // Percona Server node: a password (requirepass) + optional LDAP auth.
  valkey: {
    label: 'Valkey',
    slug: 'valkey',
    sub: 'Valkey (standalone)',
    color: '#7c3aed',
    icon: 'Database',
    singleton: false,
    ports: true, // connectable — a Traffic Sim node links to it
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', valkeyMajor: '9.1', valkeyVersion: '',
      rootPassword: '', useLdap: false, pmmNodeId: '', useProxy: false,
      exportEnabled: false, exportHostPort: 0,
    },
  },
  // Linux Client — a bare systemd host on any OS image dbcanvas supports, with no
  // product installed and no PMM monitoring. A jump box / test client for reaching
  // the stack's other nodes. Hosts are named linuxclient1, linuxclient2, … (no
  // zero-padded dash, unlike every other type) via plainSequentialLabel below.
  linuxclient: {
    label: 'Linux Client',
    slug: 'linuxclient',
    sub: 'Bare OS host — no product, no monitoring',
    color: '#64748b',
    icon: 'Server',
    singleton: false,
    ports: false,
    plainSequentialLabel: true,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }, { id: 'debian', label: 'Debian' }],
    defaults: { os: 'oraclelinux', osVersion: '9', useProxy: false },
  },
  // Traffic Sim — the "Valkey Traffic Lab" live demo app (background agents +
  // a web map). Runs dbcanvas's own first-party image, not an OS/DB image, so it
  // carries no os/osVersion/arch fields at all. Links to a standalone Valkey node
  // or a Valkey Cluster frame via a drawn association line (see endpointKind/
  // tryConnect) — its dashboard port is published to the host (like PMM), so no
  // VNC desktop is needed to reach it.
  trafficsim: {
    label: 'Traffic Sim',
    slug: 'trafficsim',
    sub: 'Valkey Traffic Lab — live demo app',
    color: '#f97316',
    icon: 'Flask',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'trafficsim', label: 'dbcanvas-trafficsim' }],
    defaults: {},
  },
  // Hotel Sim — the "MongoDB Hotel Reservation Lab" live demo app (ten background
  // agents + a web dashboard). Runs dbcanvas's own first-party image, not an OS/DB
  // image, so it carries no os/osVersion/arch fields at all. Links to a standalone
  // PS MongoDB node, a PS MongoDB replica-set frame, or a PS MongoDB sharded-cluster
  // frame via a drawn association line (see endpointKind/tryConnect) — its
  // dashboard port is published to the host (like PMM), so no VNC desktop is
  // needed to reach it.
  hotelsim: {
    label: 'Hotel Sim',
    slug: 'hotelsim',
    sub: 'MongoDB Hotel Reservation Lab — live demo app',
    color: '#f97316',
    icon: 'Flask',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'hotelsim', label: 'dbcanvas-hotelsim' }],
    defaults: {},
  },
  // Airline Sim — the "MySQL Airline Reservation Lab" live demo app (ten background
  // agents + a web dashboard) driving a 200-route reservation workload against a
  // 2000-aircraft fleet. Runs dbcanvas's own first-party image, not an OS/DB image,
  // so it carries no os/osVersion/arch fields at all. Links to a standalone Percona
  // Server node, a MySQL replication frame, a PXC cluster frame, or a
  // ProxySQL/HAProxy node or cluster fronting one of the latter two, via a drawn
  // association line (see endpointKind/tryConnect) — its dashboard port is
  // published to the host (like PMM), so no VNC desktop is needed to reach it.
  airlinesim: {
    label: 'Airline Sim',
    slug: 'airlinesim',
    sub: 'MySQL Airline Reservation Lab — live demo app',
    color: '#f97316',
    icon: 'Flask',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'airlinesim', label: 'dbcanvas-airlinesim' }],
    defaults: {},
  },
  // Car Rental Sim — the "PostgreSQL Car Rental Lab" live demo app (ten background
  // agents + a web dashboard) driving a 180-location rental workload against a
  // 2000-vehicle fleet. Runs dbcanvas's own first-party image, not an OS/DB image,
  // so it carries no os/osVersion/arch fields at all. Links to a standalone
  // PostgreSQL node, a Patroni/repmgr/Spock cluster frame, or an HAProxy node
  // fronting one of the latter three, via a drawn association line (see
  // endpointKind/tryConnect) — its dashboard port is published to the host (like
  // PMM), so no VNC desktop is needed to reach it.
  carsim: {
    label: 'Car Rental Sim',
    slug: 'carsim',
    sub: 'PostgreSQL Car Rental Lab — live demo app',
    color: '#0ea5e9',
    icon: 'Flask',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'carsim', label: 'dbcanvas-carsim' }],
    defaults: {},
  },
  // MarketChaos — the "Unoptimized MySQL Challenge": a fictional stock-exchange
  // demo app deliberately deployed with bad indexes, queries, and transaction
  // patterns for a learner to diagnose and fix. Runs dbcanvas's own first-party
  // image, not an OS/DB image, so it carries no os/osVersion/arch fields at all.
  // Links to a standalone Percona Server node, a direct PXC member node, a PXC
  // cluster or MySQL replication frame, or an HAProxy node fronting one of the
  // latter two, via a drawn association line (see endpointKind/tryConnect) — its
  // dashboard port is published to the host (like PMM), so no VNC desktop is
  // needed to reach it.
  // All in One — ONE container running many database instances side by side,
  // instead of one product per node. It carries no connection endpoints
  // (ports:false) on purpose: every relationship an instance has (PMM, LDAP,
  // OpenBao, the instance a proxy fronts) is a drop-down inside the node's form,
  // not a line drawn on the canvas. Instance options live in node.aioInstances;
  // see app/aio.go and pages/AllInOne.jsx.
  aio: {
    label: 'All in One',
    slug: 'aio',
    sub: 'Every database feature in one container',
    color: '#7c3aed',
    icon: 'Server',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', useProxy: false,
      aioPsMajor: '8.0', aioPsVersion: '', aioPxcMajor: '8.0', aioPxcVersion: '',
      aioPsmdbMajor: '8.0', aioValkeyMajor: '9.1', aioProxysqlMajor: '2',
      aioInstances: [],
    },
  },
  marketchaos: {
    label: 'Unoptimized MySQL Challenge',
    slug: 'marketchaos',
    sub: 'MarketChaos — stock-exchange performance troubleshooting lab',
    color: '#dc2626',
    icon: 'Flask',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'marketchaos', label: 'dbcanvas-marketchaos' }],
    defaults: {},
  },
  // Stock Market Sim — a stock-exchange app with full browser CRUD, a live
  // dashboard and a printable report. Distinct from MarketChaos above, which is
  // a MySQL performance-tuning *challenge* (deliberately bad indexes, no CRUD,
  // no report); this one is a working application you operate. Runs dbcanvas's
  // own first-party image, so no os/osVersion/arch fields. Two ways to reach a
  // database: linked to a standalone node on the canvas via an association line
  // (see endpointKind/tryConnect), or a manual connection typed into the form —
  // which needs no line at all and can point at a database outside the stack.
  // Its dashboard port is published to the host (like PMM), so no VNC desktop
  // is needed to reach it.
  stocksim: {
    label: 'Stock Market Sim',
    slug: 'stocksim',
    sub: 'Stock exchange app on MySQL, PostgreSQL, MongoDB or Valkey',
    color: '#14b8a6',
    icon: 'Flask',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'stocksim', label: 'dbcanvas-stocksim' }],
    defaults: {
      ssMode: 'linked', ssEngine: 'mysql', ssTLS: 'prefer', ssDatabase: 'stocksim',
      ssWorkingSet: '', ssThreads: 0,
      ssIdleTxn: '', ssExtraTables: 0, ssTempTables: 'off',
      ssLockContention: 'off', ssScanQueries: 0, ssWritePressure: 'off',
    },
  },
}

// ---------------------------------------------------------- PXC cluster frames
const PXC_NODE_W = 116
const PXC_NODE_H = 78
const FRAME_TITLE = 32
const FRAME_PAD = 14
const FRAME_GAP = 12

// layoutFrame derives a frame's size and lays its member nodes out in a row.
function layoutFrame(frame, frameNodes) {
  const n = Math.max(1, frameNodes.length)
  const w = FRAME_PAD * 2 + n * PXC_NODE_W + (n - 1) * FRAME_GAP
  const h = FRAME_TITLE + FRAME_PAD * 2 + PXC_NODE_H
  const positioned = frameNodes.map((nd, i) => ({
    ...nd,
    x: frame.x + FRAME_PAD + i * (PXC_NODE_W + FRAME_GAP),
    y: frame.y + FRAME_TITLE + FRAME_PAD,
  }))
  return { frame: { ...frame, w, h }, nodes: positioned }
}

// layoutPSMDBFrame lays out a sharded cluster as a grouped grid: a top row with
// the mongos router + the config-server RS members, then one column per shard
// (its replica-set members stacked below). Sizes adapt to the member count so it
// fits both the standard (13-node) and minimum (5-node) setups; the single-row
// layoutFrame is unusable here.
function layoutPSMDBFrame(frame, frameNodes) {
  // Stable ordering independent of array order: derive columns/rows from role.
  const mongos = frameNodes.filter((n) => n.role === 'mongos')
  const config = frameNodes.filter((n) => n.role === 'config')
  const shardIdx = [...new Set(frameNodes.filter((n) => n.role === 'shard').map((n) => n.shard))].sort((a, b) => a - b)
  const shards = shardIdx.map((s) => frameNodes.filter((n) => n.role === 'shard' && n.shard === s))
  const colW = PXC_NODE_W + FRAME_GAP
  const rowH = PXC_NODE_H + FRAME_GAP
  // columns: max(top row = 1 mongos + config members, shard columns).
  const ncols = Math.max(1 + config.length, shards.length, 3)
  const w = FRAME_PAD * 2 + ncols * PXC_NODE_W + (ncols - 1) * FRAME_GAP
  // rows: 1 top row + the tallest shard replica set.
  const maxShardRows = shards.reduce((m, s) => Math.max(m, s.length), 0)
  const nrows = 1 + maxShardRows
  const h = FRAME_TITLE + FRAME_PAD * 2 + nrows * PXC_NODE_H + (nrows - 1) * FRAME_GAP
  const ox = frame.x + FRAME_PAD
  const oy = frame.y + FRAME_TITLE + FRAME_PAD
  const positioned = []
  // Top row: mongos at col 0, config RS at cols 1..n.
  mongos.forEach((nd) => positioned.push({ ...nd, x: ox, y: oy }))
  config.forEach((nd, i) => positioned.push({ ...nd, x: ox + (i + 1) * colW, y: oy }))
  // Shard columns: each shard a column, members stacked in the rows below.
  shards.forEach((members, s) => {
    members.forEach((nd, r) => positioned.push({ ...nd, x: ox + s * colW, y: oy + (r + 1) * rowH }))
  })
  // Preserve original order for any node not matched (defensive).
  const placedIds = new Set(positioned.map((n) => n.id))
  frameNodes.forEach((nd) => { if (!placedIds.has(nd.id)) positioned.push({ ...nd, x: ox, y: oy }) })
  return { frame: { ...frame, w, h }, nodes: positioned }
}

// relayoutFrame picks the right layout for a frame type.
function relayoutFrame(frame, frameNodes) {
  return frame.type === 'psmdb' ? layoutPSMDBFrame(frame, frameNodes) : layoutFrame(frame, frameNodes)
}

// nextClusterName → pxc-cluster-NN, unique across all PXC frames (from 00).
function nextClusterName(frames) {
  let max = -1
  for (const f of frames) {
    const m = (f.label || '').match(/^pxc-cluster-(\d+)$/)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `pxc-cluster-${String(max + 1).padStart(2, '0')}`
}

// nextPXCName → lowest pxcNN not already used by any PXC node in the stack.
function nextPXCName(usedSet) {
  for (let i = 1; ; i++) {
    const name = `pxc${String(i).padStart(2, '0')}`
    if (!usedSet.has(name)) return name
  }
}

// nextNamedCluster → "<prefix>-NN" unique across the frames (from 00).
function nextNamedCluster(frames, prefix) {
  let max = -1
  const re = new RegExp(`^${prefix}-(\\d+)$`)
  for (const f of frames) {
    const m = (f.label || '').match(re)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `${prefix}-${String(max + 1).padStart(2, '0')}`
}

// nextMemberName → lowest "<prefix>NN" not already used by any node in the stack.
function nextMemberName(usedSet, prefix) {
  for (let i = 1; ; i++) {
    const name = `${prefix}${String(i).padStart(2, '0')}`
    if (!usedSet.has(name)) return name
  }
}

// Per-frame-type presentation: accent color and the description line.
const FRAME_COLORS = { pxc: '#a855f7', proxysql: '#f59e0b', mysql: '#2563eb', innodb: '#0891b2', mariadbrepl: '#c0765a', mariadbgalera: '#a85d43', mysqlcerepl: '#00758f', mysqlceinnodb: '#005d72', psmdb: '#10b981', psmrs: '#059669', patroni: '#336791', repmgr: '#0e7490', spock: '#dc2626', valkeycluster: '#7c3aed', k3d: '#326ce5' }

// A SeaweedFS node creates up to ten buckets, so several databases can share one object store
// without sharing a bucket. Every consumer picks which one it uses; "" means the node's first.
export const MAX_SEAWEED_BUCKETS = 10

// seaweedBucketsOf is a SeaweedFS node's bucket list. `bucket` is the older single-bucket field,
// kept as the fallback (and as the default bucket) for designs saved before the list existed.
// forEdit keeps blanks, so a freshly added row stays editable.
export function seaweedBucketsOf(n, forEdit) {
  const list = (n?.buckets && n.buckets.length ? n.buckets : [n?.bucket || '']).map((b) => b ?? '')
  return forEdit ? list : list.map((b) => b.trim()).filter(Boolean)
}

function validBucketName(b) {
  const s = (b || '').trim()
  return /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(s) && !/(\.\.|\.-|-\.)/.test(s)
}

// The Percona operators a K3D frame can install (PostgreSQL is discovered by `make versions` but
// not deployable yet).
export const K3D_OPERATOR_LABEL = { pxc: 'PXC operator', ps: 'MySQL (PS) operator', psmdb: 'MongoDB operator', pg: 'PostgreSQL operator', cnpg: 'CloudNativePG', pgo: 'Crunchy PGO' }

// typeColor maps a node/frame type to its canvas color so a toolbar "add" button can
// be tinted to match the node/frame it creates. addBtnStyle turns that into inline
// styles (disabled buttons keep the tint but the shared disabled:opacity-50 fades it).
const typeColor = (t) => FRAME_COLORS[t] || NODE_TYPES[t]?.color || null
const addBtnStyle = (t) => {
  const c = typeColor(t)
  return c ? { backgroundColor: c, borderColor: c, color: '#fff' } : undefined
}
const frameColor = (f) => FRAME_COLORS[f?.type] || '#a855f7'

// Member-name prefixes for the MariaDB / MySQL Community cluster frames. Having one
// table means the add-cluster builder and the add-member button cannot disagree
// about what a member of a given frame type is called — or what type it is.
const UPSTREAM_FRAME_PREFIX = { mariadbrepl: 'mariadb', mariadbgalera: 'galera', mysqlcerepl: 'mysqlce', mysqlceinnodb: 'myidc' }

// osLabel is the OS line on a node's canvas card. It compacts "Oracle Linux 9" to "OL9" — the
// cards are small, and this is the same shortening pxcOSLabel does for the nodes that carry their
// own os/osVersion. The node's *form* still shows the full name (it renders osOptions directly).
const osLabel = (type, os) => {
  const label = (NODE_TYPES[type]?.osOptions.find((o) => o.id === os)?.label) || os || ''
  return label.replace(/^Oracle Linux\s*/, 'OL')
}

// pxcOSLabel formats a node/frame's OS compactly for the canvas — "OL9", "Ubuntu 24.04". The
// cards are small and the OS is the least interesting thing on them; the full name still appears
// in the node's form.
const PXC_OS_NAMES = { oraclelinux: 'OL', ubuntu: 'Ubuntu', debian: 'Debian' }
const pxcOSLabel = (f) => {
  const os = PXC_OS_NAMES[f?.os] || f?.os || ''
  const ver = f?.osVersion || ''
  // "OL" + "9" reads as one token (OL9); the others keep the space (Ubuntu 24.04).
  return os === 'OL' ? `${os}${ver}` : [os, ver].filter(Boolean).join(' ')
}

// ENGINE_SHORT is what a deployed node calls its engine on the canvas, in front of the version it
// actually deployed with: "PS 8.4.10-10", "PMM 3.3.1". Long marketing names ("Percona Monitoring &
// Management") do not fit and say less than the version does.
const ENGINE_SHORT = {
  pxc: 'PXC', ps: 'PS', mysql: 'PS', innodb: 'PS',
  psm: 'PSMDB', psmdb: 'PSMDB', psmrs: 'PSMDB',
  pg: 'PG', patroni: 'PG', repmgr: 'PG', spock: 'PG',
  proxysql: 'ProxySQL', haproxy: 'HAProxy',
  valkey: 'Valkey', valkeycluster: 'Valkey',
  pmm: 'PMM', openbao: 'OpenBao', keycloak: 'Keycloak',
  seaweedfs: 'SeaweedFS', sambaad: 'Samba', vnc: 'Ubuntu', watchtower: 'Watchtower', k3d: 'k3s',
  orchestrator: 'Orchestrator',
}

// frameDeployedLabel is the same for a cluster frame: the version its members actually deployed
// with (they share one engine, so the first member that has been probed speaks for the frame).
const frameDeployedLabel = (f, members, depByNode) => {
  for (const m of members) {
    const l = deployedLabel(m.type, depByNode[m.id])
    if (l) return l
  }
  return ''
}

// deployedLabel is "<engine> <version>" once a node is running and its version has been probed
// (dep.config.serverVersion, see app/nodeversion.go), else "" — callers fall back to the
// design-time description.
const deployedLabel = (type, dep) => {
  const v = dep?.config?.serverVersion
  return v ? `${ENGINE_SHORT[type] || ''} ${v}`.trim() : ''
}

// pxcVersionLabel formats a PXC frame's version for display (minor if pinned,
// else the major series, e.g. "Percona XtraDB Cluster 8.0").
const pxcVersionLabel = (f) => `Percona XtraDB Cluster ${f?.pxcVersion || f?.pxcMajor || ''}`.trim()

// Frames whose members are a primary plus read-only secondaries. Grouped because
// three places need the same answer: the member's description, the greyed accent on
// a secondary, and which added members default to 'secondary'.
export const REPL_FRAME_TYPES = new Set(['mysql', 'mariadbrepl', 'mysqlcerepl'])

// frameMemberSub is the one-line description under a cluster member's name on the
// canvas.
//
// Extracted and exported so the render smoke test can assert every frame type
// answers for itself. It used to default to 'Galera data node', which silently
// mislabelled the members of every frame type added afterwards — MariaDB and MySQL
// replication members were being described as Galera data nodes.
export function frameMemberSub(f, n, kids = []) {
  if (n?.role === 'arbitrator') return 'Arbitrator · garbd'
  switch (f?.type) {
    case 'proxysql': return 'ProxySQL'
    case 'pxc': return 'Galera data node'
    case 'mariadbgalera': return 'Galera data node'
    case 'mysql':
    case 'mariadbrepl':
    case 'mysqlcerepl': return n?.role === 'primary' ? 'Primary' : 'Secondary · read-only'
    case 'innodb':
    case 'mysqlceinnodb': return f.replMode === 'groupreplication' ? 'GR member' : 'Cluster member'
    case 'psmdb': return n?.role === 'mongos' ? 'mongos router' : n?.role === 'config' ? 'config server' : `shard ${n?.shard} member`
    case 'psmrs': return 'replica-set member'
    case 'patroni': return 'Patroni node'
    case 'repmgr': return 'PostgreSQL + repmgr'
    case 'spock': return 'PostgreSQL + Spock'
    case 'valkeycluster': return 'Valkey shard'
    case 'k3d': return kids.indexOf(n) === 0 ? 'k3s server' : 'k3s agent'
    default: return 'Cluster member'
  }
}

// frameVersionLabel: the description line for a cluster-frame type.
const frameVersionLabel = (f) => {
  if (f?.type === 'proxysql') return `ProxySQL ${f?.proxysqlVersion || f?.proxysqlMajor || ''}`.trim()
  if (f?.type === 'mysql') return `Percona Server ${f?.psVersion || f?.psMajor || ''} replication`.trim()
  if (f?.type === 'innodb') return `${f?.replMode === 'groupreplication' ? 'Group Replication' : 'InnoDB Cluster'}${f?.pdpsRepo ? ` · ${f.pdpsRepo}` : ''}`
  if (f?.type === 'mariadbrepl') return `MariaDB ${f?.mariadbVersion || f?.mariadbMajor || ''} replication`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'mariadbgalera') return `MariaDB ${f?.mariadbVersion || f?.mariadbMajor || ''} Galera`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'mysqlcerepl') return `MySQL ${f?.mysqlceVersion || f?.mysqlceMajor || ''} replication`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'mysqlceinnodb') return `MySQL ${f?.mysqlceVersion || f?.mysqlceMajor || ''} · ${f?.replMode === 'groupreplication' ? 'Group Replication' : 'InnoDB Cluster'}`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'psmdb') return `PS MongoDB ${f?.psmdbVersion || f?.psmdbMajor || ''} sharded · ${f?.psmdbSetup === 'minimum' ? 'minimum' : 'standard'}`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'psmrs') return `PS MongoDB ${f?.psmdbVersion || f?.psmdbMajor || ''} replica set`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'patroni') return `Percona PostgreSQL ${f?.pgVersion || f?.pgMajor || ''} · Patroni`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'repmgr') return `PostgreSQL ${f?.pgVersion || f?.pgMajor || ''} · repmgr (PGDG)`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'spock') return `PostgreSQL ${f?.pgVersion || f?.pgMajor || ''} · Spock multi-master`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'valkeycluster') return `Valkey Cluster ${f?.valkeyVersion || f?.valkeyMajor || ''}`.trim()
  if (f?.type === 'k3d') return `Kubernetes (k3s via k3d)${K3D_OPERATOR_LABEL[f?.k3dOperator] ? ` · ${K3D_OPERATOR_LABEL[f.k3dOperator]}` : ''}`
  return pxcVersionLabel(f)
}

// ProxySQL implementation-mode options depend on the linked backend type: PXC
// (proxysql-admin singlewrite/loadbal) vs MySQL replication (primary/rwsplit). The
// "wrong" set is never shown — they switch with the associated cluster.
const PROXY_MODE_OPTS = {
  pxc: [{ id: 'singlewrite', label: 'single writer (default)' }, { id: 'loadbal', label: 'load balancer' }],
  mysql: [{ id: 'rwsplit', label: 'read/write split' }, { id: 'primary', label: 'primary only (all to primary)' }],
}
const proxyModeOpts = (backendType) => PROXY_MODE_OPTS[backendType === 'mysql' ? 'mysql' : 'pxc']

// nodeOSLabel renders a free node's OS line; ProxySQL carries its own os/version
// (like a PXC frame), other nodes map via their osOptions.
const nodeOSLabel = (n) => (n.type === 'proxysql' || n.type === 'ps' || n.type === 'pg' || n.type === 'psm' || n.type === 'haproxy' || n.type === 'orchestrator' || n.type === 'vnc' || n.type === 'linuxclient' || n.type === 'valkey' || n.type === 'aio' ? pxcOSLabel(n) : osLabel(n.type, n.os))

// Auto-numbered per-type labels: a non-singleton node is named "<slug>-NN" with
// NN zero-padded from 01 and increasing per node type (pmm-01, pmm-02, …, and in
// future psmysql-01, psmysql-02, …). These labels become the node hostnames in
// the Intranet DNS / FQDNs. The Intranet singleton keeps its plain label.
function nextLabel(type, nodes) {
  const def = NODE_TYPES[type]
  if (def.singleton) return def.label
  const base = def.slug || type
  // linuxclient1, linuxclient2, … — no dash, no zero-padding (unlike every other type).
  if (def.plainSequentialLabel) {
    const re = new RegExp(`^${base}(\\d+)$`)
    let max = 0
    for (const n of nodes) {
      if (n.type !== type) continue
      const m = (n.label || '').match(re)
      if (m) max = Math.max(max, parseInt(m[1], 10))
    }
    return `${base}${max + 1}`
  }
  const re = new RegExp(`^${base}-(\\d+)$`)
  let max = 0
  for (const n of nodes) {
    if (n.type !== type) continue
    const m = (n.label || '').match(re)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `${base}-${String(max + 1).padStart(2, '0')}`
}

// Small SVG progress ring (upper-right of a provisioning node).
function ProgressRing({ percent = 0, size = 24 }) {
  const r = (size - 5) / 2
  const c = 2 * Math.PI * r
  const off = c * (1 - Math.max(0, Math.min(100, percent)) / 100)
  const k = size / 2
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} title={`${percent}%`}>
      <circle cx={k} cy={k} r={r} fill="var(--surface)" stroke="var(--surface2)" strokeWidth="2.5" />
      <circle cx={k} cy={k} r={r} fill="none" stroke="var(--warning)" strokeWidth="2.5" strokeLinecap="round"
        strokeDasharray={c} strokeDashoffset={off} transform={`rotate(-90 ${k} ${k})`} />
    </svg>
  )
}

const STATUS_TONE = { draft: 'muted', deployed: 'success', expired: 'danger' }

export default function StackDesigner() {
  const [stacks, setStacks] = useState([])
  const [openId, setOpenId] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setError('')
    try {
      setStacks(await stackApi.list())
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  if (openId != null) {
    return <StackEditor stackId={openId} onBack={() => { setOpenId(null); load() }} />
  }

  return (
    <StackList
      stacks={stacks}
      loading={loading}
      error={error}
      onOpen={setOpenId}
      onCreated={(s) => setOpenId(s.id)}
      onChanged={load}
    />
  )
}

// ---------------------------------------------------------------- list view

function ttlLabel(id) {
  return TTL_OPTIONS.find((t) => t.id === id)?.label ?? id
}

function expiresIn(iso) {
  if (!iso) return 'never expires'
  const ms = new Date(iso) - new Date()
  if (ms <= 0) return 'expired'
  const h = Math.floor(ms / 3.6e6)
  if (h >= 24) return `expires in ${Math.floor(h / 24)}d`
  if (h >= 1) return `expires in ${h}h`
  return `expires in ${Math.max(1, Math.floor(ms / 6e4))}m`
}

function StackList({ stacks, loading, error, onOpen, onCreated, onChanged }) {
  const [showNew, setShowNew] = useState(false)

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Database Stacks</h2>
          <p className="text-sm text-muted">Design, deploy, and manage container stacks.</p>
        </div>
        <Button onClick={() => setShowNew(true)}>
          <Icon.Plus size={16} /> New stack
        </Button>
      </div>

      {error && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}

      {loading ? (
        <div className="py-10 text-center text-muted">Loading…</div>
      ) : stacks.length === 0 ? (
        <Card>
          <div className="py-10 text-center text-muted">
            No stacks yet. Create one to start designing.
          </div>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          {stacks.map((s) => (
            <Card key={s.id} className="transition hover:border-primary">
              <div className="flex items-start justify-between gap-2">
                <button onClick={() => onOpen(s.id)} className="min-w-0 text-left">
                  <div className="truncate text-sm font-semibold text-fg">{s.name}</div>
                  <div className="mt-0.5 text-xs text-muted">{expiresIn(s.expiresAt)}</div>
                </button>
                <Badge tone={STATUS_TONE[s.status] || 'muted'}>{s.status}</Badge>
              </div>
              <div className="mt-3 flex items-center justify-between">
                <Badge tone="primary">{ttlLabel(s.ttl)}</Badge>
                <div className="flex gap-1">
                  <Button size="sm" variant="outline" onClick={() => onOpen(s.id)}>Open</Button>
                  <ConfirmButton
                    size="sm"
                    variant="ghost"
                    title="Delete stack (tears down containers)"
                    confirmLabel="Delete?"
                    onConfirm={async () => { await stackApi.remove(s.id); onChanged() }}
                  >
                    <Icon.Trash size={16} />
                  </ConfirmButton>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}

      {showNew && (
        <NewStackModal
          onClose={() => setShowNew(false)}
          onCreated={(s) => { setShowNew(false); onCreated(s) }}
        />
      )}
    </div>
  )
}

function NewStackModal({ onClose, onCreated }) {
  const [name, setName] = useState('')
  const [ttl, setTtl] = useState('24h')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const s = await stackApi.create(name.trim() || 'Untitled stack', ttl)
      onCreated(s)
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-4 text-sm font-semibold">New stack</h3>
        {error && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}
        <form onSubmit={submit} className="space-y-3">
          <Field label="Name">
            <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} placeholder="My database stack" autoFocus />
          </Field>
          <Field label="Lifetime" hint="The stack and its containers are torn down when this elapses.">
            <select className={inputCls} value={ttl} onChange={(e) => setTtl(e.target.value)}>
              {TTL_OPTIONS.map((t) => (
                <option key={t.id} value={t.id}>{t.label}</option>
              ))}
            </select>
          </Field>
          <div className="flex justify-end gap-2 pt-1">
            <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create'}</Button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}

// -------------------------------------------------------------- editor view

// The Infrastructure Library holds ~25 entries across 8 categories — far more than
// fits a 200px column, so everything below the fold used to be a scroll-hunt. The
// palette persists which categories you collapsed and the handful of entries you
// actually reach for, per browser.
const PALETTE_KEY = 'dbcanvas-palette'
const RECENT_MAX = 5
// Extra search terms per node type — the words people actually type that appear in no
// label or category ("redis" for Valkey, "k8s" for K3D, "mongo" for the PSMDB entries).
const PALETTE_ALIASES = {
  valkey: 'redis cache kv', valkeycluster: 'redis cache kv',
  k3d: 'k8s kubernetes k3s cluster',
  psmdb: 'mongo mongodb shard', psmrs: 'mongo mongodb replica', psm: 'mongo mongodb',
  pxc: 'galera mysql cluster', ps: 'mysql percona', mysql: 'replication source replica',
  innodb: 'mysql group replication gr',
  pg: 'postgres postgresql', patroni: 'postgres postgresql ha', repmgr: 'postgres postgresql ha',
  spock: 'postgres postgresql logical replication',
  proxysql: 'mysql lb load balancer', haproxy: 'lb load balancer',
  orchestrator: 'mysql topology failover failure detection recovery',
  pmm: 'monitoring metrics grafana', openbao: 'vault secrets',
  sambaad: 'ldap active directory domain', keycloak: 'sso oidc identity',
  seaweedfs: 's3 object storage', vnc: 'desktop gui ubuntu',
  watchtower: 'updates upgrade', intranet: 'dns gateway core',
  linuxclient: 'client host bare vm jump box test',
  trafficsim: 'demo simulation city map live traffic',
  hotelsim: 'demo simulation hotel reservation booking mongo mongodb',
  airlinesim: 'demo simulation airline flight reservation booking mysql pxc',
  carsim: 'demo simulation car rental booking postgres postgresql patroni repmgr spock',
  marketchaos: 'demo simulation stock market exchange trading mysql pxc performance tuning index challenge unoptimized',
  stocksim: 'demo simulation stock market portfolio trading crud report dashboard external mysql postgres mongodb valkey',
}
function loadPalettePrefs() {
  try { return { collapsed: [], recent: [], ...JSON.parse(localStorage.getItem(PALETTE_KEY) || '{}') } }
  catch { return { collapsed: [], recent: [] } }
}

function StackEditor({ stackId, onBack }) {
  const [stack, setStack] = useState(null)
  const [error, setError] = useState('')
  const [nodes, setNodes] = useState([])
  const [edges, setEdges] = useState([])
  const [frames, setFrames] = useState([])
  const [view, setView] = useState({ x: 40, y: 20, z: 1 })
  // Node palette: docked to the left by default; can be undocked into a floating,
  // resizable panel (drag by its header, resize via the corner handle) and re-docked.
  const [paletteDocked, setPaletteDocked] = useState(true)
  const [palettePos, setPalettePos] = useState({ x: 24, y: 24 })
  const [paletteQuery, setPaletteQuery] = useState('')
  const [collapsed, setCollapsed] = useState(() => loadPalettePrefs().collapsed)
  const [recent, setRecent] = useState(() => loadPalettePrefs().recent)
  const [selected, setSelected] = useState(null)
  const [menu, setMenu] = useState(null)
  const [connect, setConnect] = useState(null)
  const [linkPrompt, setLinkPrompt] = useState(null) // ProxySQL↔ProxySQL: choose flow direction
  const [replPrompt, setReplPrompt] = useState(null) // member↔member: choose replication direction/type
  const [confirmDel, setConfirmDel] = useState(null) // confirm deleting a deployed node/cluster
  const [saveState, setSaveState] = useState('saved') // saved | saving
  const [deployments, setDeployments] = useState([])
  const [issues, setIssues] = useState(null) // validate results panel
  const [busy, setBusy] = useState('') // 'validate' | 'deploy' | ''
  const [configNode, setConfigNode] = useState(null) // node whose profile is shown
  const [deployPanel, setDeployPanel] = useState('hidden') // 'open' | 'min' | 'hidden'
  // The architecture this installation targets. Nodes no longer carry one — an
  // installation builds images for a single DOCKER_PLATFORM — so the canvas cards
  // label them from the catalogue instead of from the design.
  const [platform, setPlatform] = useState('')
  const [fileDrag, setFileDrag] = useState(false) // host files are being dragged over the canvas
  const [dropNode, setDropNode] = useState(null) // node id currently under a file drag
  const [drop, setDrop] = useState(null) // dropped files awaiting a destination choice
  const [xfer, setXfer] = useState(null) // the transfer dialog, once a destination is picked
  const [fileMgr, setFileMgr] = useState(null) // { nodeId, label } while the file manager is open
  const xferAbort = useRef(null)
  const [flash, setFlash] = useState(null) // transient bottom toast ({ tone, text })
  const { openTerminal } = useTerminals()
  const { system } = useSettings() // instance-wide: the node-upload ceiling

  const wrapRef = useRef(null)
  const dragRef = useRef(null)
  const counter = useRef(0)
  const uid = (p) => `${p}-${Date.now().toString(36)}-${++counter.current}`

  useEffect(() => {
    try { localStorage.setItem(PALETTE_KEY, JSON.stringify({ collapsed, recent })) } catch { /* */ }
  }, [collapsed, recent])

  const refs = useRef({})
  refs.current = { nodes, edges, frames, view }
  const stackRef = useRef(null)
  stackRef.current = stack
  const lastSaved = useRef('')

  // load
  useEffect(() => {
    let alive = true
    stackApi.imagesCatalog().then((c) => setPlatform(c.platform || '')).catch(() => { /* the cards fall back */ })
    stackApi.get(stackId).then((s) => {
      if (!alive) return
      setStack(s)
      setDeployments(s.deployments || [])
      const d = s.design || {}
      const nz = d.nodes || []
      const ez = d.edges || []
      const fz = d.frames || []
      const vw = d.view || { x: 40, y: 20, z: 1 }
      setNodes(nz)
      setEdges(ez)
      setFrames(fz)
      setView(vw)
      lastSaved.current = JSON.stringify({ nodes: nz, edges: ez, frames: fz, view: vw })
    }).catch((err) => setError(err.message))
    return () => { alive = false }
  }, [stackId])

  // poll deployment state (does NOT touch the local design while editing)
  useEffect(() => {
    const t = setInterval(async () => {
      try {
        const s = await stackApi.get(stackId)
        setDeployments(s.deployments || [])
        setStack((prev) => (prev ? { ...prev, status: s.status } : prev))
      } catch {
        // ignore transient errors
      }
    }, 3000)
    return () => clearInterval(t)
  }, [stackId])

  const depByNode = {}
  for (const d of deployments) depByNode[d.nodeId] = d

  // While a deployment is in progress the node set is frozen: no adding or removing
  // nodes (the server rejects it too). Option/position edits stay live. Cleared once
  // every node finishes provisioning.
  const deploying = busy === 'deploy' || deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')

  // auto-open the deployment console while anything is provisioning, but never
  // override the user's minimized choice.
  useEffect(() => {
    if (deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')) {
      setDeployPanel((p) => (p === 'hidden' ? 'open' : p))
    }
  }, [deployments])

  // debounced autosave — only when the design actually differs from the last
  // saved snapshot (so the 3s status poll never triggers a save).
  useEffect(() => {
    if (!stackRef.current) return
    const cur = JSON.stringify({ nodes, edges, frames, view })
    if (cur === lastSaved.current) return
    setSaveState('saving')
    const t = setTimeout(async () => {
      try {
        await stackApi.update(stackRef.current.id, stackRef.current.name, { nodes, edges, frames, view })
        lastSaved.current = cur
      } catch { /* keep dirty; will retry on next change */ }
      setSaveState('saved')
    }, 600)
    return () => clearTimeout(t)
  }, [nodes, edges, frames, view])

  const getWorld = useCallback((cx, cy) => {
    const rect = wrapRef.current.getBoundingClientRect()
    return screenToWorld(rect, refs.current.view, cx, cy)
  }, [])

  // rectOf resolves a connection endpoint id to its rectangle — a free node uses
  // the fixed node size, a PXC cluster frame its own geometry.
  const rectOf = useCallback((id) => {
    const n = refs.current.nodes.find((x) => x.id === id)
    // A cluster member (inside a frame) uses the small member-card geometry; a free
    // node uses the full node size.
    if (n) return n.frameId ? { x: n.x, y: n.y, w: PXC_NODE_W, h: PXC_NODE_H } : { x: n.x, y: n.y, w: NODE_W, h: NODE_H }
    const f = refs.current.frames.find((x) => x.id === id)
    if (f) return { x: f.x, y: f.y, w: f.w, h: f.h }
    return null
  }, [])

  // Endpoints that expose connection ports: free nodes whose type opts in
  // (def.ports), and the cluster frames in CONNECTABLE_FRAMES. Cluster member
  // nodes connect via their frame, not individually.
  function hitPort(world, excludeId) {
    let best = null
    let bestD = SNAP
    const consider = (id, r) => {
      if (id === excludeId) return
      for (const port of PORTS) {
        const d = dist(world, portPoint(r, port))
        if (d < bestD) { bestD = d; best = { id, port } }
      }
    }
    for (const n of refs.current.nodes) {
      if (n.frameId) {
        // PXC and Percona Server replication members expose ports for cross-cluster
        // replication links; other members (ProxySQL, InnoDB) do not.
        if (n.type === 'pxc' || n.type === 'mysql') consider(n.id, { x: n.x, y: n.y, w: PXC_NODE_W, h: PXC_NODE_H })
        continue
      }
      if (!NODE_TYPES[n.type]?.ports) continue
      consider(n.id, { x: n.x, y: n.y, w: NODE_W, h: NODE_H })
    }
    for (const f of refs.current.frames) {
      if (CONNECTABLE_FRAMES.has(f.type)) consider(f.id, { x: f.x, y: f.y, w: f.w, h: f.h })
    }
    return best
  }

  // global pointer move/up
  useEffect(() => {
    function onMove(e) {
      const d = dragRef.current
      if (!d) return
      if (d.kind === 'pan') {
        setView((v) => ({ ...v, x: d.ox + (e.clientX - d.sx), y: d.oy + (e.clientY - d.sy) }))
        return
      }
      if (d.kind === 'palette') {
        setPalettePos({ x: Math.max(0, d.ox + (e.clientX - d.sx)), y: Math.max(0, d.oy + (e.clientY - d.sy)) })
        return
      }
      const w = getWorld(e.clientX, e.clientY)
      if (d.kind === 'node') {
        setNodes((ns) => ns.map((n) => (n.id === d.id ? { ...n, x: w.x + d.offx, y: w.y + d.offy } : n)))
      } else if (d.kind === 'frame') {
        const nx = w.x + d.offx, ny = w.y + d.offy
        const frame = refs.current.frames.find((f) => f.id === d.id)
        setFrames((fs) => fs.map((f) => (f.id === d.id ? { ...f, x: nx, y: ny } : f)))
        if (frame) {
          const mine = refs.current.nodes.filter((n) => n.frameId === d.id)
          const laid = new Map(relayoutFrame({ ...frame, x: nx, y: ny }, mine).nodes.map((n) => [n.id, n]))
          setNodes((ns) => ns.map((n) => laid.get(n.id) || n))
        }
      } else if (d.kind === 'connect') {
        const tgt = hitPort(w, d.fromId)
        const src = portPoint(rectOf(d.fromId), d.fromPort)
        const to = tgt ? portPoint(rectOf(tgt.id), tgt.port) : w
        d.lastTarget = tgt
        setConnect({ from: src, to, targetId: tgt?.id ?? null, targetPort: tgt?.port ?? null })
      }
    }
    function onUp() {
      const d = dragRef.current
      if (d?.kind === 'connect') {
        const t = d.lastTarget
        if (t && t.id !== d.fromId) {
          tryConnect({ node: d.fromId, port: d.fromPort }, { node: t.id, port: t.port })
        }
      }
      dragRef.current = null
      setConnect(null)
    }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => {
      removeEventListener('pointermove', onMove)
      removeEventListener('pointerup', onUp)
    }
  }, [getWorld, rectOf])

  // wheel zoom
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    function onWheel(e) {
      e.preventDefault()
      const rect = el.getBoundingClientRect()
      setView((v) => zoomAt(v, e.clientX - rect.left, e.clientY - rect.top, e.deltaY))
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
    // Re-run once the canvas actually mounts: StackEditor renders a "Loading…"
    // placeholder while stack is null, so wrapRef.current is null on first mount and
    // the listener would otherwise never attach (breaking wheel zoom).
  }, [stack])

  useEffect(() => {
    if (!flash) return
    const t = setTimeout(() => setFlash(null), 4000)
    return () => clearTimeout(t)
  }, [flash])

  // A drag abandoned outside the window (Escape, or released over another app)
  // never reaches the canvas handlers, so clear the drop affordances globally.
  useEffect(() => {
    const end = () => { setFileDrag(false); setDropNode(null) }
    addEventListener('dragend', end)
    addEventListener('drop', end)
    return () => { removeEventListener('dragend', end); removeEventListener('drop', end) }
  }, [])

  // delete key
  useEffect(() => {
    function onKey(e) {
      if (e.key === 'Escape') {
        setMenu(null)
        setDrop(null)
        // The transfer dialog is not Escape-dismissible: while it runs, closing
        // it would orphan a copy the user can no longer see or cancel; once it
        // has finished, the outcome is the thing they have to acknowledge.
      }
      if (e.key !== 'Delete') return
      const t = e.target
      if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return
      if (selected) { e.preventDefault(); deleteSelected() }
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  })

  // interactions
  function startPan(e) {
    if (e.button !== 0) return
    setSelected(null)
    setMenu(null)
    dragRef.current = { kind: 'pan', sx: e.clientX, sy: e.clientY, ox: view.x, oy: view.y }
  }
  function startNode(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu(null)
    const w = getWorld(e.clientX, e.clientY)
    const n = nodes.find((x) => x.id === id)
    dragRef.current = { kind: 'node', id, offx: n.x - w.x, offy: n.y - w.y }
  }
  function startFrame(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'frame', id })
    setMenu(null)
    const w = getWorld(e.clientX, e.clientY)
    const f = frames.find((x) => x.id === id)
    dragRef.current = { kind: 'frame', id, offx: f.x - w.x, offy: f.y - w.y }
  }
  function selectFrameNode(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu(null)
  }
  function startConnect(e, ownerId, port) {
    if (e.button !== 0) return
    e.stopPropagation()
    setMenu(null)
    const src = portPoint(rectOf(ownerId), port)
    dragRef.current = { kind: 'connect', fromId: ownerId, fromPort: port, lastTarget: null }
    setConnect({ from: src, to: src, targetId: null, targetPort: null })
  }
  function openMenu(e, id) {
    e.preventDefault()
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu({ x: e.clientX, y: e.clientY, id })
  }

  // copyExecCommand puts `docker exec -it <container> bash` on the clipboard and
  // flashes what happened — a menu item that copies silently gives the operator
  // no way to tell a success from a browser that refused the clipboard.
  async function copyExecCommand(name) {
    const cmd = `docker exec -it ${name} bash`
    setFlash(await copyText(cmd) ? { tone: 'ok', text: `Copied: ${cmd}` } : { tone: 'err', text: `Could not reach the clipboard. Command: ${cmd}` })
  }

  // --- drag files from the host onto a node -------------------------------
  // Only a *running* node can take a drop: the copy goes through the engine's
  // put-archive on a live container. Everything else (a stopped node, a node
  // that was never deployed, empty canvas) refuses the drag so the cursor says
  // "no" rather than the drop failing after the fact.
  const canDrop = (id) => depByNode[id]?.state === 'running'

  // isFileDrag distinguishes a drag from the desktop from the editor's own
  // pointer drags — dataTransfer.types carries 'Files' only for the former.
  const isFileDrag = (e) => Array.from(e.dataTransfer?.types || []).includes('Files')

  function nodeDragOver(e, id) {
    if (!isFileDrag(e)) return
    if (!fileDrag) setFileDrag(true)
    if (!canDrop(id)) return
    // preventDefault on dragover is what marks an element as a drop target;
    // without it the browser navigates to the file on drop.
    e.preventDefault()
    e.stopPropagation()
    e.dataTransfer.dropEffect = 'copy'
    if (dropNode !== id) setDropNode(id)
  }

  function nodeDragLeave(e, id) {
    // A drag moving over child elements fires leave/enter pairs; only clear when
    // the pointer has actually left the card.
    if (e.currentTarget.contains(e.relatedTarget)) return
    setDropNode((cur) => (cur === id ? null : cur))
  }

  async function nodeDrop(e, id) {
    if (!isFileDrag(e) || !canDrop(id)) return
    e.preventDefault()
    e.stopPropagation()
    setFileDrag(false)
    setDropNode(null)
    const { x, y } = { x: e.clientX, y: e.clientY }
    let files = []
    try {
      files = await collectDroppedFiles(e.dataTransfer)
    } catch (err) {
      setDrop({ id, x, y, phase: 'error', message: `Could not read the dropped items: ${err.message}` })
      return
    }
    if (files.length === 0) {
      setDrop({ id, x, y, phase: 'error', message: 'Nothing to copy — the drop held no files.' })
      return
    }
    // Refuse an over-size drop here rather than after pushing it up the wire.
    // The server enforces the same ceiling (app/nodeupload.go); this only saves
    // the upload, so a stale limit costs a round trip, never correctness.
    const total = files.reduce((n, f) => n + f.file.size, 0)
    const max = system.maxUploadBytes
    if (max > 0 && total > max) {
      setDrop({
        id, x, y, phase: 'error',
        message: `That drop is ${fmtBytes(total)} — over this instance's ${fmtBytes(max)} limit for node uploads. An admin can raise it in Settings.`,
      })
      return
    }
    setDrop({ id, x, y, phase: 'choose', files, total })
  }

  // runUpload hands the drop off to the transfer dialog: the little picker
  // closes, and everything from here is reported in a modal the user has to
  // acknowledge. A copy that can run for minutes needs somewhere to live that
  // an accidental click cannot dismiss.
  async function runUpload(dest) {
    const d = drop
    if (!d?.files) return
    setDrop(null)
    const ctrl = new AbortController()
    xferAbort.current = ctrl
    const base = {
      nodeId: d.id,
      label: nodes.find((n) => n.id === d.id)?.label || 'node',
      dest,
      count: d.files.length,
      total: d.total || 0,
    }
    setXfer({ ...base, phase: 'uploading', sent: 0, wire: 0 })
    try {
      const r = await stackApi.nodeUpload(stack.id, d.id, dest, d.files, {
        signal: ctrl.signal,
        onProgress: (sent, wire) => setXfer((x) => (x && x.phase === 'uploading' ? { ...x, sent, wire } : x)),
        // The bytes are away; the server is now extracting the tar into the
        // container. No percentage exists for that half, and cancelling it
        // would leave the destination half-written — so the dialog switches to
        // an indeterminate state with Cancel withdrawn.
        onSent: () => setXfer((x) => (x && x.phase === 'uploading' ? { ...x, phase: 'extracting' } : x)),
      })
      setXfer((x) => (x ? { ...x, phase: 'done', count: r?.files?.length ?? x.count } : x))
    } catch (err) {
      setXfer((x) => (x ? { ...x, phase: err.aborted ? 'cancelled' : 'error', message: err.message } : x))
    } finally {
      xferAbort.current = null
    }
  }

  function cancelUpload() {
    xferAbort.current?.abort()
  }

  // --- association links (read refs.current so they're correct when called from
  // the captured pointer-up handler) ---
  // endpointKind classifies a connectable endpoint:
  //   'pxc'           — PXC cluster frame (source only)
  //   'proxysql'      — standalone ProxySQL node (1 incoming, many outgoing)
  //   'proxysql-frame'— ProxySQL cluster frame (1 incoming from PXC, no outgoing)
  // Member nodes inside a frame are not linkable (no ports).
  // 'backend' = a PXC or MySQL cluster frame (source only); 'proxysql' = standalone
  // ProxySQL node; 'proxysql-frame' = ProxySQL cluster frame.
  // 'replmember' = a PXC or Percona Server replication member node (a source/replica
  // for a cross-cluster replication link).
  function endpointKind(id) {
    const n = refs.current.nodes.find((x) => x.id === id)
    if (n) {
      if (n.type === 'proxysql' && !n.frameId) return 'proxysql'
      if (n.type === 'haproxy' && !n.frameId) return 'haproxy'
      if ((n.type === 'pxc' || n.type === 'mysql') && n.frameId) return 'replmember'
      if (n.type === 'valkey') return 'valkey'
      if (n.type === 'trafficsim') return 'trafficsim'
      if (n.type === 'psm') return 'psm'
      if (n.type === 'hotelsim') return 'hotelsim'
      if (n.type === 'ps' && !n.frameId) return 'ps'
      if (n.type === 'pg' && !n.frameId) return 'pg'
      // Standalone MariaDB and MySQL CE had no kind at all until Stock Market
      // Sim became able to drive them; nothing else links to them yet.
      if (n.type === 'mariadb' && !n.frameId) return 'mariadb'
      if (n.type === 'mysqlce' && !n.frameId) return 'mysqlce'
      if (n.type === 'airlinesim') return 'airlinesim'
      if (n.type === 'carsim') return 'carsim'
      if (n.type === 'marketchaos') return 'marketchaos'
      if (n.type === 'stocksim') return 'stocksim'
      return null
    }
    const f = refs.current.frames.find((x) => x.id === id)
    if (f) {
      if (f.type === 'pxc' || f.type === 'mysql') return 'backend'
      if (f.type === 'proxysql') return 'proxysql-frame'
      if (f.type === 'patroni') return 'patroni'
      if (f.type === 'repmgr') return 'repmgr'
      if (f.type === 'spock') return 'spock'
      if (f.type === 'valkeycluster') return 'valkeycluster'
      if (f.type === 'psmrs') return 'psmrs'
      if (f.type === 'psmdb') return 'psmdb'
      // MySQL-family frames other than PXC/MySQL replication, and the
      // Kubernetes frame. Each keeps its own kind rather than joining
      // 'backend', because only Stock Market Sim accepts them and the rules
      // below have to stay able to tell them apart.
      if (f.type === 'innodb') return 'innodb'
      if (f.type === 'mariadbrepl') return 'mariadbrepl'
      if (f.type === 'mariadbgalera') return 'mariadbgalera'
      if (f.type === 'mysqlcerepl') return 'mysqlcerepl'
      if (f.type === 'mysqlceinnodb') return 'mysqlceinnodb'
      if (f.type === 'k3d') return 'k3d'
      return null
    }
    return null
  }
  // createFlow adds a directed edge from→to (arrow at the destination). The
  // destination may have at most ONE incoming flow; a PXC frame source may have at
  // most ONE outgoing flow (opts.singleOutgoing). Rejected (no arrow) otherwise.
  function createFlow(fromEnd, toEnd, opts = {}) {
    const E = refs.current.edges
    if (E.some((ed) => ed.to.node === toEnd.node)) return // destination already receives
    if (opts.singleOutgoing && E.some((ed) => ed.from.node === fromEnd.node)) return // source already sends
    if (E.some((ed) => (ed.from.node === fromEnd.node && ed.to.node === toEnd.node) || (ed.from.node === toEnd.node && ed.to.node === fromEnd.node))) return
    const id = uid('e')
    setEdges((es) => [...es, { id, from: fromEnd, to: toEnd, type: 'directional' }])
    setSelected({ kind: 'edge', id })
  }
  // tryConnect applies the association rules to a dropped connection.
  function tryConnect(e1, e2) {
    const k1 = endpointKind(e1.node)
    const k2 = endpointKind(e2.node)
    if (!k1 || !k2) return
    // No second link between the same pair.
    if (refs.current.edges.some((ed) => (ed.from.node === e1.node && ed.to.node === e2.node) || (ed.from.node === e2.node && ed.to.node === e1.node))) return
    const isProxyDest = (k) => k === 'proxysql' || k === 'proxysql-frame'
    // PXC/MySQL backend frame → ProxySQL node/cluster frame (frame is always the
    // source, max 1 outgoing).
    if (k1 === 'backend' && isProxyDest(k2)) return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'backend' && isProxyDest(k1)) return createFlow(e2, e1, { singleOutgoing: true })
    // Patroni, repmgr, or Spock cluster frame → HAProxy node (frame is the source,
    // max 1 outgoing; HAProxy takes a single incoming via the createFlow dest
    // guard, so a node can front at most one of these three PostgreSQL topologies).
    if (k1 === 'patroni' && k2 === 'haproxy') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'patroni' && k1 === 'haproxy') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'repmgr' && k2 === 'haproxy') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'repmgr' && k1 === 'haproxy') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'spock' && k2 === 'haproxy') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'spock' && k1 === 'haproxy') return createFlow(e2, e1, { singleOutgoing: true })
    // PXC or MySQL-replication frame → HAProxy node. HAProxy fronts exactly one
    // cluster (Patroni, PXC, or MySQL replication) — its single incoming (createFlow
    // dest guard) enforces the mutual exclusivity.
    if (k1 === 'backend' && k2 === 'haproxy') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'backend' && k1 === 'haproxy') return createFlow(e2, e1, { singleOutgoing: true })
    // Standalone Valkey node OR Valkey Cluster frame → Traffic Sim node (the data
    // source flows to its consumer, same shape as Patroni cluster frame → HAProxy
    // above; a Traffic Sim node links to exactly one target, single incoming).
    if (k1 === 'valkey' && k2 === 'trafficsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'valkey' && k1 === 'trafficsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'valkeycluster' && k2 === 'trafficsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'valkeycluster' && k1 === 'trafficsim') return createFlow(e2, e1, { singleOutgoing: true })
    // Standalone PS MongoDB node OR a PSMDB replica-set/sharded-cluster frame →
    // Hotel Sim node (same shape as the Valkey → Traffic Sim rules above; a Hotel
    // Sim node links to exactly one target, single incoming).
    if (k1 === 'psm' && k2 === 'hotelsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'psm' && k1 === 'hotelsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'psmrs' && k2 === 'hotelsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'psmrs' && k1 === 'hotelsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'psmdb' && k2 === 'hotelsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'psmdb' && k1 === 'hotelsim') return createFlow(e2, e1, { singleOutgoing: true })
    // Standalone Percona Server node, a PXC/MySQL backend frame (direct), or a
    // ProxySQL/HAProxy node or cluster fronting one of the latter two → Airline Sim
    // node (same shape as the rules above; an Airline Sim node links to exactly one
    // target, single incoming). A 'backend' frame's singleOutgoing already covers
    // "connect Airline Sim directly OR front it with a proxy, not both" for free.
    if (k1 === 'ps' && k2 === 'airlinesim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'ps' && k1 === 'airlinesim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'backend' && k2 === 'airlinesim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'backend' && k1 === 'airlinesim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'haproxy' && k2 === 'airlinesim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'haproxy' && k1 === 'airlinesim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'proxysql' && k2 === 'airlinesim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'proxysql' && k1 === 'airlinesim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'proxysql-frame' && k2 === 'airlinesim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'proxysql-frame' && k1 === 'airlinesim') return createFlow(e2, e1, { singleOutgoing: true })
    // Standalone PostgreSQL node, a Patroni/repmgr/Spock cluster frame (direct), or
    // an HAProxy node fronting one of the latter three → Car Rental Sim node (same
    // shape as the Airline Sim rules above; a Car Rental Sim node links to exactly
    // one target, single incoming). Each of 'pg'/'patroni'/'repmgr'/'spock'/
    // 'haproxy' already carries its own singleOutgoing semantics, so "connect Car
    // Rental Sim directly OR front it with HAProxy, not both" falls out for free —
    // there is no ProxySQL rule here at all: ProxySQL is MySQL-family only.
    if (k1 === 'pg' && k2 === 'carsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'pg' && k1 === 'carsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'patroni' && k2 === 'carsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'patroni' && k1 === 'carsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'repmgr' && k2 === 'carsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'repmgr' && k1 === 'carsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'spock' && k2 === 'carsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'spock' && k1 === 'carsim') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'haproxy' && k2 === 'carsim') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'haproxy' && k1 === 'carsim') return createFlow(e2, e1, { singleOutgoing: true })
    // Standalone Percona Server node, a single PXC member node linked directly
    // ('replmember' — bypasses the cluster frame on purpose, for challenges about
    // an app that never load-balances at all), a PXC/MySQL backend frame, or an
    // HAProxy node fronting one of the latter two → MarketChaos node (same shape
    // as the Airline Sim rules above; a MarketChaos node links to exactly one
    // target, single incoming). No ProxySQL rule here — out of scope for V1.
    if (k1 === 'ps' && k2 === 'marketchaos') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'ps' && k1 === 'marketchaos') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'replmember' && k2 === 'marketchaos') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'replmember' && k1 === 'marketchaos') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'backend' && k2 === 'marketchaos') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'backend' && k1 === 'marketchaos') return createFlow(e2, e1, { singleOutgoing: true })
    if (k1 === 'haproxy' && k2 === 'marketchaos') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'haproxy' && k1 === 'marketchaos') return createFlow(e2, e1, { singleOutgoing: true })
    // Any database → Stock Market Sim node. This is the one sim that is not
    // tied to an engine, so its rule is a table rather than a pair of lines per
    // type; SS_LINKABLE_KINDS mirrors stockSimStandaloneTargets and
    // stockSimFrameTargets in app/stocksim.go. singleOutgoing is what enforces
    // "one Stock Market Sim node drives exactly one database" — four separate
    // applications means four nodes, each with its own line and its own
    // dashboard. A node set to a manual connection needs no line at all and
    // simply never uses this rule.
    if (SS_LINKABLE_KINDS.has(k1) && k2 === 'stocksim') return createFlow(e1, e2, { singleOutgoing: true })
    if (SS_LINKABLE_KINDS.has(k2) && k1 === 'stocksim') return createFlow(e2, e1, { singleOutgoing: true })
    // ProxySQL node ↔ ProxySQL node: ask which way the data flows.
    if (k1 === 'proxysql' && k2 === 'proxysql') { setLinkPrompt({ e1, e2 }); return }
    // Cluster member ↔ cluster member (PXC/Percona Server, different frames): a
    // cross-cluster replication link. Ask for async direction or bidirectional.
    if (k1 === 'replmember' && k2 === 'replmember') {
      const n1 = refs.current.nodes.find((x) => x.id === e1.node)
      const n2 = refs.current.nodes.find((x) => x.id === e2.node)
      if (!n1 || !n2 || n1.frameId === n2.frameId) return // same cluster — already replicating
      setReplPrompt({ e1, e2 })
      return
    }
    // Everything else (frame↔frame, ProxySQL cluster frame as source, node↔cluster
    // frame, self) is not allowed.
  }
  // createReplEdge adds a cross-cluster replication link. mode "async" → From is the
  // source, To the replica (arrow at the replica). mode "bidir" → both replicate
  // from each other (double-headed). One link per node pair (tryConnect rejects a
  // second); change direction/type later from the link's Properties panel.
  function createReplEdge(fromEnd, toEnd, mode) {
    const id = uid('e')
    setEdges((es) => [...es, { id, from: fromEnd, to: toEnd, type: mode }])
    setSelected({ kind: 'edge', id })
  }

  // mutations
  const patchNode = (id, patch) => setNodes((ns) => ns.map((n) => (n.id === id ? { ...n, ...patch } : n)))
  const patchFrame = (id, patch) => setFrames((fs) => fs.map((f) => (f.id === id ? { ...f, ...patch } : f)))
  const patchEdge = (id, patch) => setEdges((es) => es.map((e) => (e.id === id ? { ...e, ...patch } : e)))
  // askDelete opens the confirmation modal (used before destroying a *deployed*
  // node/cluster, whose containers + volumes get torn down in real time).
  function askDelete(kind, label, onConfirm, count) {
    setConfirmDel({ kind, label, count, onConfirm })
  }
  function deleteNode(id) {
    if (deploying) return
    const node = nodes.find((n) => n.id === id)
    // PS MongoDB sharded-cluster topology is fixed: members can't be removed
    // individually (delete the whole frame to remove the cluster).
    if (node?.type === 'psmdb') return
    // A deployed node has live containers/volumes — confirm before deleting.
    if (depByNode[id]) { askDelete('node', node?.label || 'node', () => doDeleteNode(id)); return }
    doDeleteNode(id)
  }
  function doDeleteNode(id) {
    // A PXC member belongs to a frame: re-lay the frame after removing it (and
    // drop the frame entirely if it was the last node), so the menu/manager
    // delete behaves like the frame's own remove control.
    const node = nodes.find((n) => n.id === id)
    if (node?.frameId) {
      const siblings = nodes.filter((n) => n.frameId === node.frameId)
      if (siblings.length <= 1) { doDeleteFrame(node.frameId); return }
      const r = relayout(node.frameId, frames, nodes.filter((n) => n.id !== id))
      setFrames(r.frames)
      setNodes(r.nodes)
      // Drop any replication links attached to the removed member.
      setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
      setSelected((s) => (s?.kind === 'node' && s.id === id ? { kind: 'frame', id: node.frameId } : s))
      return
    }
    setNodes((ns) => ns.filter((n) => n.id !== id))
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'node' && s.id === id ? null : s))
  }
  function deleteEdge(id) {
    setEdges((es) => es.filter((e) => e.id !== id))
    setSelected((s) => (s?.kind === 'edge' && s.id === id ? null : s))
  }
  function deleteFrame(id) {
    if (deploying) return
    // Confirm when the cluster has deployed members (their containers + volumes go).
    const deployedMembers = nodes.filter((n) => n.frameId === id && depByNode[n.id]).length
    if (deployedMembers > 0) {
      const label = frames.find((f) => f.id === id)?.label || 'cluster'
      askDelete('frame', label, () => doDeleteFrame(id), deployedMembers)
      return
    }
    doDeleteFrame(id)
  }
  function doDeleteFrame(id) {
    const memberIds = new Set(nodes.filter((n) => n.frameId === id).map((n) => n.id))
    setNodes((ns) => ns.filter((n) => n.frameId !== id))
    setFrames((fs) => fs.filter((f) => f.id !== id))
    // Drop any association lines attached to the frame (or its member nodes).
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id && !memberIds.has(e.from.node) && !memberIds.has(e.to.node)))
    setSelected((s) => (s && (s.id === id) ? null : s))
  }
  function deleteSelected() {
    if (selected?.kind === 'node') deleteNode(selected.id)
    else if (selected?.kind === 'edge') deleteEdge(selected.id)
    else if (selected?.kind === 'frame') deleteFrame(selected.id)
  }

  // --- PXC cluster frame operations ---
  // Re-lay a frame's member nodes (positions derive from the frame geometry).
  function relayout(frameId, framesArr, nodesArr) {
    const frame = framesArr.find((f) => f.id === frameId)
    if (!frame) return { frames: framesArr, nodes: nodesArr }
    const mine = nodesArr.filter((n) => n.frameId === frameId)
    const others = nodesArr.filter((n) => n.frameId !== frameId)
    const r = relayoutFrame(frame, mine)
    return {
      frames: framesArr.map((f) => (f.id === frameId ? r.frame : f)),
      nodes: [...others, ...r.nodes],
    }
  }
  function addPXCCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'pxc', label: nextClusterName(frames), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', pxcMajor: '8.0', pxcVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false, gtid: true,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'pxc').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextPXCName(used)
      used.add(name)
      newNodes.push({ id: uid('pxc'), type: 'pxc', label: name, frameId: fid, role: 'regular', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function addPXCNode(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'pxc').map((n) => n.label))
    const name = nextPXCName(used)
    const newNode = { id: uid('pxc'), type: 'pxc', label: name, frameId, role: 'regular', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
    const r = relayout(frameId, frames, [...nodes, newNode])
    setFrames(r.frames)
    setNodes(r.nodes)
  }
  function newProxySQLMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'proxysql').map((n) => n.label))
    return { id: uid('proxysql'), type: 'proxysql', label: nextMemberName(used, 'proxysql'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  function addProxySQLCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'proxysql', label: nextNamedCluster(frames, 'proxysql-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', proxysqlMajor: '2', proxysqlVersion: '',
      mode: 'singlewrite', pmmNodeId: '', useProxy: false,
    }
    const used = new Set(nodes.filter((n) => n.type === 'proxysql').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'proxysql')
      used.add(name)
      newNodes.push({ id: uid('proxysql'), type: 'proxysql', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newMySQLMember(frameId, role) {
    const used = new Set(nodes.filter((n) => n.type === 'mysql').map((n) => n.label))
    return { id: uid('mysql'), type: 'mysql', label: nextMemberName(used, 'mysql'), frameId, role: role || 'secondary', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  function addMySQLCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'mysql', label: nextNamedCluster(frames, 'psrepl'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', psMajor: '8.0', psVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false, gtid: true, replMode: 'async',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'mysql').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'mysql')
      used.add(name)
      newNodes.push({ id: uid('mysql'), type: 'mysql', label: name, frameId: fid, role: i === 0 ? 'primary' : 'secondary', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newInnoDBMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'innodb').map((n) => n.label))
    return { id: uid('innodb'), type: 'innodb', label: nextMemberName(used, 'innodb'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  function addInnoDBCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'innodb', label: nextNamedCluster(frames, 'innodb'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', pdpsRepo: '', replMode: 'innodbcluster',
      rootPassword: '', pmmNodeId: '', useProxy: false, mysqlRouter: true,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'innodb').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'innodb')
      used.add(name)
      newNodes.push({ id: uid('innodb'), type: 'innodb', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // The four upstream cluster kinds share one shape — a frame carrying the version
  // and options, plus N member nodes of the same type — so one builder serves them
  // all. Only the defaults and the member count differ.
  function addUpstreamCluster(type, prefix, count, frameDefaults, withRoles) {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type, label: nextNamedCluster(frames, prefix), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      ...frameDefaults,
    }
    const used = new Set(nodes.filter((n) => n.type === type).map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < count; i++) {
      const name = nextMemberName(used, prefix)
      used.add(name)
      const node = { id: uid(prefix), type, label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
      // Galera and Group Replication are multi-master: no member is "the primary",
      // and the seed is chosen positionally by the provisioner.
      if (withRoles) node.role = i === 0 ? 'primary' : 'secondary'
      newNodes.push(node)
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // newUpstreamMember adds one member to an existing MariaDB / MySQL Community
  // frame. Without this the generic fallback in addFrameMember would create a
  // `pxc` node inside, say, a MariaDB frame — the member type must match the
  // frame's, because the provisioner selects members by n.Type == frame.Type.
  function newUpstreamMember(frameId, type) {
    const prefix = UPSTREAM_FRAME_PREFIX[type]
    const used = new Set(nodes.filter((n) => n.type === type).map((n) => n.label))
    const node = { id: uid(prefix), type, label: nextMemberName(used, prefix), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
    // Replication frames keep exactly one primary, so an added member is a
    // secondary. Galera and Group Replication members carry no role at all.
    if (REPL_FRAME_TYPES.has(type)) node.role = 'secondary'
    return node
  }
  const addMariaDBCluster = () => addUpstreamCluster('mariadbrepl', UPSTREAM_FRAME_PREFIX['mariadbrepl'], 3,
    { mariadbMajor: '11.4', mariadbVersion: '', gtid: true, replMode: 'async' }, true)
  const addMariaDBGaleraCluster = () => addUpstreamCluster('mariadbgalera', UPSTREAM_FRAME_PREFIX['mariadbgalera'], 3,
    { mariadbMajor: '11.4', mariadbVersion: '' }, false)
  const addMySQLCECluster = () => addUpstreamCluster('mysqlcerepl', UPSTREAM_FRAME_PREFIX['mysqlcerepl'], 3,
    { mysqlceMajor: '8.4', mysqlceVersion: '', gtid: true, replMode: 'async' }, true)
  const addMySQLCEInnoDBCluster = () => addUpstreamCluster('mysqlceinnodb', UPSTREAM_FRAME_PREFIX['mysqlceinnodb'], 3,
    { mysqlceMajor: '8.4', mysqlceVersion: '', replMode: 'innodbcluster', mysqlRouter: true }, false)

  // A K3D cluster's members are the k3s nodes k3d creates: the first is the server, the rest
  // agents. Default 1 node (a k3s cluster is perfectly happy single-node), resizable to 3.
  function newK3DMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'k3d').map((n) => n.label))
    return { id: uid('k3s'), type: 'k3d', label: nextMemberName(used, 'k3s'), frameId, x: 0, y: 0 }
  }
  function addK3DCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'k3d', label: nextNamedCluster(frames, 'k3d'), x: fx, y: fy, w: 0, h: 0,
      k3dNodes: 1, k3dCpus: 4, k3dMemoryGb: 8, k3dK3sVersion: '',
      k3dOperator: '', k3dOperatorVer: '', k3dNamespace: 'default',
      k3dProxy: 'haproxy', k3dExposePxc: 'clusterip', k3dExposeHaproxy: 'loadbalancer', k3dExposeProxysql: 'loadbalancer',
      k3dSharding: false, k3dExposeReplset: 'clusterip', k3dExposeMongos: 'loadbalancer',
      k3dExposePg: 'clusterip', k3dExposePgbouncer: 'loadbalancer',
      k3dPgoInstances: 2, k3dPgoStorageGb: 1, k3dPgoVersion: '',
      k3dClusterType: 'group-replication', k3dExposeMysql: 'clusterip', k3dExposeRouter: 'loadbalancer',
      k3dPmmTokenTtlValue: 365, k3dPmmTokenTtlUnit: 'days',
      k3dDebug: false, k3dDebugPort: 40000, k3dDebugNoPublish: false,
      pmmNodeId: '', seaweedfsNodeId: '',
    }
    const r = relayout(fid, [...frames, frame], [...nodes, newK3DMember(fid)])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // psmdbMembers builds the member nodes for a PS MongoDB sharded cluster of the
  // given setup, always 1 mongos + 3 shards + a config-server RS:
  //   standard → 3-node config RS + 3 shards × 3-node RS (13 nodes)
  //   minimum  → 1 config server + 3 single-node shard RS    (5 nodes)
  // Member labels: mongos (role mongos), cfgNN (role config), sNrM (role shard).
  // Labels become DNS hostnames and must be unique stack-wide, so a second sharded frame
  // takes a "-2" suffix (mongos-2, cfg1-2, s0r1-2, …) — the lowest suffix that clears every
  // label already used by psmdb members outside this frame. `others` = the nodes the new
  // members will join (excludes the frame's own members when rebuilding).
  function psmdbMembers(fid, setup, others) {
    const rs = setup === 'minimum' ? 1 : 3
    const cfgN = setup === 'minimum' ? 1 : 3
    const base = ['mongos']
    for (let i = 0; i < cfgN; i++) base.push(`cfg${i + 1}`)
    for (let s = 0; s < 3; s++) {
      for (let r = 0; r < rs; r++) base.push(`s${s}r${r + 1}`)
    }
    const used = new Set(others.filter((x) => x.type === 'psmdb' && x.frameId !== fid).map((x) => x.label))
    let suffix = ''
    for (let i = 2; base.some((b) => used.has(`${b}${suffix}`)); i++) suffix = `-${i}`

    const mk = (label, role, shard, slot) => {
      const nd = { id: uid('psmdb'), type: 'psmdb', label: `${label}${suffix}`, frameId: fid, role, _slot: slot, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
      if (shard !== undefined) nd.shard = shard
      return nd
    }
    const out = []
    out.push(mk('mongos', 'mongos', undefined, 0)) // the "mongosh" node
    for (let i = 0; i < cfgN; i++) out.push(mk(`cfg${i + 1}`, 'config', undefined, i)) // config RS
    for (let s = 0; s < 3; s++) {
      for (let r = 0; r < rs; r++) out.push(mk(`s${s}r${r + 1}`, 'shard', s, r)) // shard RS
    }
    return out
  }
  // addMongoDBCluster builds a PS MongoDB sharded cluster frame. Topology is fixed
  // per setup (no add/remove); the setup can be switched in the frame form before
  // deploy.
  function addMongoDBCluster(setup = 'standard') {
    if (!nodes.some((n) => n.type === 'intranet')) return
    setup = setup === 'minimum' ? 'minimum' : 'standard'
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'psmdb', label: nextNamedCluster(frames, 'psmdb'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', psmdbMajor: '8.0', psmdbVersion: '',
      psmdbSetup: setup, rootPassword: '', pmmNodeId: '', useProxy: false,
      enablePBM: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...psmdbMembers(fid, setup, nodes)])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // rebuildMongoCluster swaps a PS MongoDB frame's members for a different setup
  // (standard ↔ minimum). Only allowed before deploy; replication links never
  // attach to psmdb members, so none need pruning.
  function rebuildMongoCluster(frameId, setup) {
    const frame = frames.find((f) => f.id === frameId)
    if (!frame || frame.type !== 'psmdb') return
    const others = nodes.filter((n) => n.frameId !== frameId)
    const r = relayout(frameId, frames.map((f) => (f.id === frameId ? { ...f, psmdbSetup: setup } : f)), [...others, ...psmdbMembers(frameId, setup, others)])
    setFrames(r.frames)
    setNodes(r.nodes)
  }
  function newPSMRSMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'psmrs').map((n) => n.label))
    return { id: uid('psmrs'), type: 'psmrs', label: nextMemberName(used, 'psmrs'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addMongoRSCluster builds a PS MongoDB replica-set frame with 3 members
  // (resizable 1–9). Members all run mongod in one replica set; an admin user is
  // created on the elected primary.
  function addMongoRSCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'psmrs', label: nextNamedCluster(frames, 'psmrs'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', psmdbMajor: '8.0', psmdbVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      enablePBM: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'psmrs').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'psmrs')
      used.add(name)
      newNodes.push({ id: uid('psmrs'), type: 'psmrs', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newPatroniMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'patroni').map((n) => n.label))
    return { id: uid('patroni'), type: 'patroni', label: nextMemberName(used, 'patroni'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addPatroniCluster builds a Patroni PostgreSQL cluster frame with 3 members
  // (resizable 3–7). Each member co-locates PostgreSQL + Patroni + an etcd member;
  // one node is elected leader and the rest stream as replicas.
  function addPatroniCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'patroni', label: nextNamedCluster(frames, 'patroni-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', pgMajor: '16', pgVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      usePgBackRest: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'patroni').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'patroni')
      used.add(name)
      newNodes.push({ id: uid('patroni'), type: 'patroni', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newRepmgrMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'repmgr').map((n) => n.label))
    return { id: uid('repmgr'), type: 'repmgr', label: nextMemberName(used, 'repmgr'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addRepmgrCluster builds a repmgr PostgreSQL cluster frame with 3 members
  // (resizable 3–7). Streaming replication managed by repmgr; repmgrd does failover.
  function addRepmgrCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'repmgr', label: nextNamedCluster(frames, 'repmgr-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', pgMajor: '16', pgVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      useBarman: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'repmgr').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'repmgr')
      used.add(name)
      newNodes.push({ id: uid('repmgr'), type: 'repmgr', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newSpockMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'spock').map((n) => n.label))
    return { id: uid('spock'), type: 'spock', label: nextMemberName(used, 'spock'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addSpockCluster builds a Spock PostgreSQL cluster frame with 3 members (resizable
  // 2–7). Every member is writable — full-mesh active-active via pgEdge Spock.
  function addSpockCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'spock', label: nextNamedCluster(frames, 'spock-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', pgMajor: '16', pgVersion: '',
      pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'spock').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'spock')
      used.add(name)
      newNodes.push({ id: uid('spock'), type: 'spock', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newValkeyMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'valkeycluster').map((n) => n.label))
    return { id: uid('valkey'), type: 'valkeycluster', label: nextMemberName(used, 'valkey'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addValkeyCluster builds a Valkey Cluster frame with 3 members (resizable 3–7,
  // all-master shards via valkey-cli --cluster create).
  function addValkeyCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'valkeycluster', label: nextNamedCluster(frames, 'valkey-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', valkeyMajor: '9.1', valkeyVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false, useLdap: false,
    }
    const used = new Set(nodes.filter((n) => n.type === 'valkeycluster').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'valkey')
      used.add(name)
      newNodes.push({ id: uid('valkey'), type: 'valkeycluster', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // Frame +/- buttons dispatch by frame type.
  function addFrameMember(frame) {
    if (deploying) return
    if (frame.type === 'proxysql') {
      const r = relayout(frame.id, frames, [...nodes, newProxySQLMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'mysql') {
      // Added members are secondaries (the single primary is kept).
      const r = relayout(frame.id, frames, [...nodes, newMySQLMember(frame.id, 'secondary')])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'innodb') {
      const r = relayout(frame.id, frames, [...nodes, newInnoDBMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'psmrs') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 9) return // max 9 members
      const r = relayout(frame.id, frames, [...nodes, newPSMRSMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'patroni') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7 (etcd quorum)
      const r = relayout(frame.id, frames, [...nodes, newPatroniMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'repmgr') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7
      const r = relayout(frame.id, frames, [...nodes, newRepmgrMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'spock') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7
      const r = relayout(frame.id, frames, [...nodes, newSpockMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'k3d') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 3) return // max 3 k3s nodes
      const r = relayout(frame.id, frames, [...nodes, newK3DMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'valkeycluster') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7
      const r = relayout(frame.id, frames, [...nodes, newValkeyMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (UPSTREAM_FRAME_PREFIX[frame.type]) {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 9) return // max 9
      const r = relayout(frame.id, frames, [...nodes, newUpstreamMember(frame.id, frame.type)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else {
      addPXCNode(frame.id)
    }
  }
  function removePXCNode(frameId) {
    if (deploying) return
    const mine = nodes.filter((n) => n.frameId === frameId)
    if (mine.length <= 1) return // keep at least one node
    // Patroni/repmgr need ≥3 members: never drop below 3.
    const frame = frames.find((f) => f.id === frameId)
    if ((frame?.type === 'patroni' || frame?.type === 'repmgr' || frame?.type === 'valkeycluster' || frame?.type === 'mariadbgalera' || frame?.type === 'mysqlceinnodb') && mine.length <= 3) return
    if (frame?.type === 'spock' && mine.length <= 2) return // Spock keeps ≥2 members
    const target = mine[mine.length - 1]
    // Confirm when the member being dropped is deployed (its container + volume go).
    if (depByNode[target.id]) { askDelete('node', target.label || 'node', () => removePXCNodeById(frameId, target.id)); return }
    removePXCNodeById(frameId, target.id)
  }
  function removePXCNodeById(frameId, id) {
    if (deploying) return
    const mine = nodes.filter((n) => n.frameId === frameId)
    if (mine.length <= 1) return // keep at least one node
    const r = relayout(frameId, frames, nodes.filter((n) => n.id !== id))
    setFrames(r.frames)
    setNodes(r.nodes)
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'node' && s.id === id ? { kind: 'frame', id: frameId } : s))
  }
  function addNode(type) {
    const def = NODE_TYPES[type]
    if (def.singleton && nodes.some((n) => n.type === type)) return
    // The Intranet is required first — it provides DNS/mail/LDAP/CA for the stack.
    if (type !== 'intranet' && !nodes.some((n) => n.type === 'intranet')) return
    const id = uid(type)
    const x = (-view.x + 220) / view.z
    const y = (-view.y + 160) / view.z
    // No arch is stamped, here or in any type's defaults: an installation targets one
    // Docker platform (DOCKER_PLATFORM) and `make images` builds only that, so the
    // server resolves it — see archOr. A value saved here would override that.
    setNodes((ns) => [...ns, {
      id, type, x, y, label: nextLabel(type, ns), os: def.osOptions[0].id, ...(def.defaults || {}),
    }])
    setSelected({ kind: 'node', id })
  }

  const upsertDep = (ds, d) => {
    const next = ds.filter((x) => x.nodeId !== d.nodeId)
    next.push(d)
    return next
  }

  // Flush the debounced design save so validate/deploy act on exactly what's on
  // the canvas (otherwise a just-toggled option — e.g. the cert checkbox — may not
  // have hit the server yet and a stale design gets deployed).
  async function saveNow() {
    if (!stackRef.current) return
    const cur = JSON.stringify({ nodes, edges, frames, view })
    if (cur === lastSaved.current) return
    await stackApi.update(stackRef.current.id, stackRef.current.name, { nodes, edges, frames, view })
    lastSaved.current = cur
    setSaveState('saved')
  }

  async function runValidate() {
    setBusy('validate')
    try {
      await saveNow()
      const r = await stackApi.validate(stack.id)
      setIssues(r.issues || [])
    } catch (err) {
      setIssues([{ level: 'error', message: err.message }])
    } finally {
      setBusy('')
    }
  }

  async function runDeploy() {
    setBusy('deploy')
    setIssues(null)
    try {
      await saveNow()
      const v = await stackApi.validate(stack.id)
      if (!v.ok) {
        setIssues(v.issues)
        return
      }
      const r = await stackApi.deploy(stack.id)
      setDeployments(r.deployments || [])
      setStack((p) => ({ ...p, status: 'deployed' }))
      setDeployPanel('open')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }

  async function runDestroy() {
    setBusy('destroy')
    setIssues(null)
    try {
      await stackApi.destroy(stack.id)
      setDeployments([])
      setStack((p) => ({ ...p, status: 'draft' }))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }

  async function nodeAction(nid, action) {
    try {
      const d = await stackApi.nodeAction(stack.id, nid, action)
      setDeployments((ds) => upsertDep(ds, d))
    } catch (err) {
      setError(err.message)
    }
  }

  async function showConfig(nid) {
    try {
      setConfigNode(await stackApi.getNode(stack.id, nid))
    } catch (err) {
      setError(err.message)
    }
  }

  function nodeMenuActions(id) {
    const dep = depByNode[id]
    const actions = []
    if (dep) {
      actions.push({ label: 'View config / profile', fn: () => showConfig(id) })
      if (dep.state === 'running') {
        const node = nodes.find((n) => n.id === id)
        if (node?.type === 'pmm') {
          // The PMM image runs as the unprivileged pmm user, so a plain exec is the pmm
          // console; root needs -u 0.
          actions.push({ label: 'Enter root console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node.label} · root`, user: '0' }) })
          actions.push({ label: 'Enter PMM console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node.label} · pmm` }) })
        } else {
          actions.push({ label: 'Enter root console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node?.label || 'node'} · root` }) })
        }
        actions.push({ label: 'File manager', fn: () => setFileMgr({ nodeId: id, label: node?.label || 'node' }) })
        // The same shell, but from the operator's own terminal: hand them the
        // exact `docker exec` line for this node's container.
        if (dep.containerName) {
          actions.push({ label: 'Copy docker exec command', fn: () => copyExecCommand(dep.containerName) })
        }
        actions.push({ label: 'Stop', fn: () => nodeAction(id, 'stop') })
        actions.push({ label: 'Restart', fn: () => nodeAction(id, 'restart') })
      } else if (dep.state === 'stopped' || dep.state === 'error') {
        actions.push({ label: 'Start', fn: () => nodeAction(id, 'start') })
      }
      actions.push({ sep: true })
    }
    // PS MongoDB members are part of a fixed topology — no individual delete.
    if (nodes.find((n) => n.id === id)?.type !== 'psmdb') {
      actions.push({ label: 'Delete node', danger: true, fn: () => deleteNode(id) })
    }
    return actions
  }

  if (error) {
    return (
      <div className="space-y-3">
        <Button variant="ghost" onClick={onBack}><Icon.ArrowLeft size={16} /> Back</Button>
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>
      </div>
    )
  }
  if (!stack) return <div className="py-10 text-center text-muted">Loading…</div>

  const hasIntranet = nodes.some((n) => n.type === 'intranet')

  // Node palette: categorized add-buttons (vertical). Used both docked-left and floating.
  const has = (t) => nodes.some((n) => n.type === t)
  const paletteGroups = [
    { title: 'Core', items: [
      { label: 'Intranet', type: 'intranet', onClick: () => addNode('intranet'), off: hasIntranet },
      { label: 'Samba AD DC', type: 'sambaad', onClick: () => addNode('sambaad'), off: has('sambaad') },
      { label: 'PMM3', type: 'pmm', onClick: () => addNode('pmm') },
      { label: 'Watchtower', type: 'watchtower', onClick: () => addNode('watchtower'), off: has('watchtower') },
      { label: 'Keycloak', type: 'keycloak', onClick: () => addNode('keycloak'), off: has('keycloak') },
    ] },
    { title: 'All in One', items: [
      { label: 'All in One', type: 'aio', onClick: () => addNode('aio') },
    ] },
    { title: 'MySQL', items: [
      { label: 'PXC Cluster', type: 'pxc', onClick: addPXCCluster },
      { label: 'Percona Server', type: 'ps', onClick: () => addNode('ps') },
      { label: 'PS Replication', type: 'mysql', onClick: addMySQLCluster },
      { label: 'InnoDB / GR', type: 'innodb', onClick: addInnoDBCluster },
      { label: 'Orchestrator', type: 'orchestrator', onClick: () => addNode('orchestrator') },
    ] },
    { title: 'MySQL Community', items: [
      { label: 'MySQL', type: 'mysqlce', onClick: () => addNode('mysqlce') },
      { label: 'MySQL Replication', type: 'mysqlcerepl', onClick: addMySQLCECluster },
      { label: 'InnoDB / GR', type: 'mysqlceinnodb', onClick: addMySQLCEInnoDBCluster },
    ] },
    { title: 'MariaDB', items: [
      { label: 'MariaDB', type: 'mariadb', onClick: () => addNode('mariadb') },
      { label: 'MariaDB Replication', type: 'mariadbrepl', onClick: addMariaDBCluster },
      { label: 'MariaDB Galera', type: 'mariadbgalera', onClick: addMariaDBGaleraCluster },
    ] },
    { title: 'Load Balancer', items: [
      { label: 'ProxySQL', type: 'proxysql', onClick: () => addNode('proxysql') },
      { label: 'ProxySQL Cluster', type: 'proxysql', onClick: addProxySQLCluster },
      { label: 'HAProxy', type: 'haproxy', onClick: () => addNode('haproxy') },
    ] },
    { title: 'MongoDB', items: [
      { label: 'PSMDB Sharded', type: 'psmdb', onClick: () => addMongoDBCluster() },
      { label: 'PSMDB Replica Set', type: 'psmrs', onClick: addMongoRSCluster },
      { label: 'PSMDB', type: 'psm', onClick: () => addNode('psm') },
    ] },
    { title: 'PostgreSQL', items: [
      { label: 'PostgreSQL', type: 'pg', onClick: () => addNode('pg') },
      { label: 'Patroni Cluster', type: 'patroni', onClick: addPatroniCluster },
      { label: 'repmgr Cluster', type: 'repmgr', onClick: addRepmgrCluster },
      { label: 'Spock Cluster', type: 'spock', onClick: addSpockCluster },
    ] },
    { title: 'Valkey', items: [
      { label: 'Valkey Cluster', type: 'valkeycluster', onClick: addValkeyCluster },
      { label: 'Valkey', type: 'valkey', onClick: () => addNode('valkey') },
    ] },
    { title: 'Kubernetes', items: [
      { label: 'K3D Cluster', type: 'k3d', onClick: addK3DCluster },
    ] },
    { title: 'Storage & Tools', items: [
      { label: 'SeaweedFS', type: 'seaweedfs', onClick: () => addNode('seaweedfs') },
      { label: 'OpenBao', type: 'openbao', onClick: () => addNode('openbao'), off: has('openbao') },
      { label: 'Ubuntu VNC', type: 'vnc', onClick: () => addNode('vnc'), off: has('vnc') },
      { label: 'Linux Client', type: 'linuxclient', onClick: () => addNode('linuxclient') },
    ] },
    { title: 'App Simulators (experimental)', items: [
      { label: 'Traffic Sim', type: 'trafficsim', onClick: () => addNode('trafficsim') },
      { label: 'Hotel Sim', type: 'hotelsim', onClick: () => addNode('hotelsim') },
      { label: 'Airline Sim', type: 'airlinesim', onClick: () => addNode('airlinesim') },
      { label: 'Car Rental Sim', type: 'carsim', onClick: () => addNode('carsim') },
      { label: 'Unoptimized MySQL Challenge', type: 'marketchaos', onClick: () => addNode('marketchaos') },
      { label: 'Stock Market Sim', type: 'stocksim', onClick: () => addNode('stocksim') },
    ] },
  ]

  // Flat index for the search box and the recents lookup. Labels are unique across
  // groups, so a label doubles as an entry's stable id in the persisted recents.
  const paletteItems = paletteGroups.flatMap((g) => g.items.map((it) => ({ ...it, group: g.title })))
  // Search matches label + category + node type, so "mongo" finds every PSMDB entry via
  // its category. Aliases cover the names people type that appear nowhere in the UI.
  const q = paletteQuery.trim().toLowerCase()
  const matches = (it, group) =>
    !q || `${it.label} ${group} ${it.type} ${PALETTE_ALIASES[it.type] || ''}`.toLowerCase().includes(q)

  const remember = (label) => setRecent((r) => [label, ...r.filter((l) => l !== label)].slice(0, RECENT_MAX))
  const toggleGroup = (title) =>
    setCollapsed((c) => (c.includes(title) ? c.filter((t) => t !== title) : [...c, title]))

  const paletteButton = (it, key) => {
    const disabled = it.off || (it.type !== 'intranet' && !hasIntranet) || deploying
    const reason = deploying
      ? 'Locked while deploying'
      : it.off ? `${it.label} is already on the canvas`
      : !hasIntranet && it.type !== 'intranet' ? 'Add an Intranet node first' : it.label
    return (
      <button key={key} disabled={disabled} title={reason}
        onClick={() => { remember(it.label); it.onClick() }}
        style={addBtnStyle(it.type)}
        className="flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium shadow-sm disabled:opacity-40">
        <Icon.Plus size={13} /> <span className="truncate">{it.label}</span>
      </button>
    )
  }

  // Searching flattens the tree: every match is shown regardless of its category's
  // collapsed state, since a hidden match reads as "no result".
  const recentItems = q ? [] : recent.map((l) => paletteItems.find((it) => it.label === l)).filter(Boolean)
  const hits = q ? paletteItems.filter((it) => matches(it, it.group)) : []

  const paletteBody = (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="shrink-0 px-2 pt-2">
        <div className="relative">
          <Icon.Search size={12} className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-muted" />
          <input
            value={paletteQuery}
            onChange={(e) => setPaletteQuery(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Escape') setPaletteQuery('') }}
            placeholder="Search…"
            aria-label="Search the Infrastructure Library"
            className={`${inputCls} py-1 pl-6 pr-6 text-xs`} />
          {q && (
            <button onClick={() => setPaletteQuery('')} title="Clear search"
              className="absolute right-1.5 top-1/2 -translate-y-1/2 px-1 text-[10px] text-muted hover:text-fg">✕</button>
          )}
        </div>
      </div>
      <div className="min-h-0 flex-1 space-y-3 overflow-y-auto px-2 py-2">
        {deploying && (
          <div className="rounded-md border border-warning/30 bg-warning/10 px-2 py-1.5 text-[11px] leading-snug text-warning">
            Deployment in progress — adding and removing nodes is locked until it finishes.
          </div>
        )}

        {q && (
          hits.length === 0
            ? <div className="px-1 py-3 text-center text-[11px] text-muted">No match for “{paletteQuery.trim()}”</div>
            : paletteGroups.map((g) => {
              const found = g.items.filter((it) => matches(it, g.title))
              if (found.length === 0) return null
              return (
                <div key={g.title}>
                  <div className="px-1 pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">{g.title}</div>
                  <div className="space-y-1">{found.map((it) => paletteButton(it, it.label))}</div>
                </div>
              )
            })
        )}

        {recentItems.length > 0 && (
          <div>
            <div className="flex items-center justify-between px-1 pb-1">
              <span className="text-[10px] font-semibold uppercase tracking-wide text-muted">Recently used</span>
              <button onClick={() => setRecent([])} title="Clear recently used"
                className="text-[10px] text-muted hover:text-fg">clear</button>
            </div>
            <div className="space-y-1">{recentItems.map((it) => paletteButton(it, `recent-${it.label}`))}</div>
          </div>
        )}

        {!q && paletteGroups.map((g) => {
          const shut = collapsed.includes(g.title)
          return (
            <div key={g.title}>
              <button onClick={() => toggleGroup(g.title)} aria-expanded={!shut}
                className="flex w-full items-center gap-1 px-1 pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted hover:text-fg">
                <Icon.Chevron size={12} className={`shrink-0 transition-transform ${shut ? '-rotate-90' : ''}`} />
                <span className="truncate">{g.title}</span>
                {shut && <span className="ml-auto tabular-nums opacity-70">{g.items.length}</span>}
              </button>
              {!shut && <div className="space-y-1">{g.items.map((it) => paletteButton(it, it.label))}</div>}
            </div>
          )
        })}
      </div>
    </div>
  )
  const paletteHeader = (onToggle, dockLabel, dockIcon, onDrag) => (
    <div className={`flex shrink-0 items-center justify-between border-b px-2 py-1.5 ${onDrag ? 'cursor-move' : ''}`} onPointerDown={onDrag}>
      <span className="text-xs font-semibold">Infrastructure Library</span>
      <button title={dockLabel} onClick={onToggle} className="text-muted hover:text-fg">{dockIcon}</button>
    </div>
  )

  return (
    <div className="flex h-[78vh] gap-4">
      <div className="flex min-w-0 flex-1 flex-col gap-3">
        {/* toolbar */}
        <div className="flex flex-wrap items-center gap-2 rounded-xl border bg-surface px-3 py-2">
          <Button size="sm" variant="ghost" onClick={onBack}><Icon.ArrowLeft size={16} /> Stacks</Button>
          <div className="mx-1 h-5 w-px bg-border" />
          <span className="text-sm font-semibold">{stack.name}</span>
          <Badge tone="primary">{ttlLabel(stack.ttl)}</Badge>
          <Badge tone={STATUS_TONE[stack.status] || 'muted'}>{stack.status}</Badge>
          <div className="mx-1 h-5 w-px bg-border" />
          {paletteDocked && <span className="text-xs text-muted">Add nodes from the Infrastructure Library →</span>}
          {!paletteDocked && (
            <Button size="sm" variant="outline" onClick={() => setPaletteDocked(true)}><Icon.Plus size={15} /> Palette</Button>
          )}
          <div className="mx-1 h-5 w-px bg-border" />
          <Button size="sm" variant="outline" disabled={!!busy} onClick={runValidate}>
            <Icon.Check size={15} /> {busy === 'validate' ? 'Validating…' : 'Validate'}
          </Button>
          <Button size="sm" disabled={!!busy || nodes.length === 0} onClick={runDeploy}>
            <Icon.Arrow size={15} /> {busy === 'deploy' ? 'Deploying…' : 'Deploy'}
          </Button>
          {(deployments.length > 0 || stack.status === 'deployed') && (
            <ConfirmButton size="sm" variant="outline" disabled={!!busy} confirmLabel="Destroy — sure?" onConfirm={runDestroy}>
              <Icon.Trash size={15} /> {busy === 'destroy' ? 'Destroying…' : 'Destroy'}
            </ConfirmButton>
          )}
          <div className="ml-auto flex items-center gap-3">
            <span className="text-xs text-muted">{saveState === 'saving' ? 'Saving…' : 'Saved'}</span>
            <span className="text-xs text-muted">{nodes.length} nodes · {edges.length} links</span>
            <Button size="sm" variant="ghost" onClick={() => setView({ x: 40, y: 20, z: 1 })}>
              <Icon.Move size={15} /> Reset view
            </Button>
          </div>
        </div>

        {issues && (
          <div className="rounded-xl border bg-surface p-3">
            <div className="mb-2 flex items-center justify-between">
              <span className="text-xs font-semibold text-muted">Validation</span>
              <button onClick={() => setIssues(null)} className="text-xs text-muted hover:text-fg">dismiss</button>
            </div>
            <ul className="space-y-1">
              {issues.map((it, i) => (
                <li key={i} className="flex items-center gap-2 text-sm">
                  <Badge tone={it.level === 'error' ? 'danger' : it.level === 'warning' ? 'warning' : 'success'}>{it.level}</Badge>
                  <span className="text-fg">{it.message}</span>
                </li>
              ))}
            </ul>
          </div>
        )}

        {/* canvas + node palette (docked left, or floating) */}
        <div className="flex min-h-0 flex-1 gap-3">
        {paletteDocked && (
          <div className="flex w-[200px] shrink-0 flex-col overflow-hidden rounded-xl border bg-surface">
            {paletteHeader(() => setPaletteDocked(false), 'Undock (float)', <Icon.External size={14} />, null)}
            {paletteBody}
          </div>
        )}
        <div
          ref={wrapRef}
          onPointerDown={startPan}
          onContextMenu={(e) => { e.preventDefault(); setMenu(null) }}
          // Claim file drags for the canvas so a miss lands nowhere instead of
          // making the browser navigate away from the designer to the file.
          onDragOver={(e) => { if (isFileDrag(e)) { e.preventDefault(); e.dataTransfer.dropEffect = 'none'; if (!fileDrag) setFileDrag(true) } }}
          onDragLeave={(e) => { if (!e.currentTarget.contains(e.relatedTarget)) { setFileDrag(false); setDropNode(null) } }}
          onDrop={(e) => { if (isFileDrag(e)) { e.preventDefault(); setFileDrag(false); setDropNode(null) } }}
          className="relative flex-1 overflow-hidden rounded-xl border bg-bg"
          style={{ touchAction: 'none' }}
        >
          {!paletteDocked && (
            <div className="absolute z-20 flex flex-col rounded-xl border bg-surface shadow-lg"
              onPointerDown={(e) => e.stopPropagation()}
              style={{ left: palettePos.x, top: palettePos.y, width: 210, height: 380, minWidth: 170, minHeight: 220, resize: 'both', overflow: 'hidden' }}>
              {paletteHeader(() => setPaletteDocked(true), 'Dock left', <Icon.ArrowLeft size={14} />, (e) => { e.stopPropagation(); dragRef.current = { kind: 'palette', sx: e.clientX, sy: e.clientY, ox: palettePos.x, oy: palettePos.y } })}
              {paletteBody}
            </div>
          )}
          <div
            className="pointer-events-none absolute inset-0"
            style={{
              backgroundImage: 'radial-gradient(var(--grid) 1.4px, transparent 1.4px)',
              backgroundSize: `${24 * view.z}px ${24 * view.z}px`,
              backgroundPosition: `${view.x}px ${view.y}px`,
            }}
          />
          <div className="absolute left-0 top-0 origin-top-left" style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})` }}>
            <svg className="pointer-events-none absolute left-0 top-0 overflow-visible" width="1" height="1">
              <defs>
                <marker id="stk-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                  <path d="M0,0 L10,5 L0,10 z" fill="context-stroke" />
                </marker>
              </defs>
              {edges.map((ed) => {
                const r0 = rectOf(ed.from.node)
                const r1 = rectOf(ed.to.node)
                if (!r0 || !r1) return null
                const p0 = portPoint(r0, ed.from.port)
                const p1 = portPoint(r1, ed.to.port)
                const d = edgePath(p0, ed.from.port, p1, ed.to.port)
                const on = selected?.kind === 'edge' && selected.id === ed.id
                const repl = ed.type === 'async' || ed.type === 'bidir'
                // Caption: a cross-cluster replication link, or an association line
                // (any link involving a ProxySQL or HAProxy node, or a ProxySQL cluster frame).
                const proxyNodeEnd = nodes.some((n) => (n.id === ed.from.node || n.id === ed.to.node) && (n.type === 'proxysql' || n.type === 'haproxy'))
                const proxyFrameEnd = frames.some((fr) => (fr.id === ed.from.node || fr.id === ed.to.node) && fr.type === 'proxysql')
                const caption = repl
                  ? (ed.type === 'bidir' ? 'bidirectional replication' : 'async replication')
                  : (proxyNodeEnd || proxyFrameEnd ? 'forwards SQL traffic to' : null)
                return (
                  <g key={ed.id}>
                    <path d={d} fill="none" stroke="transparent" strokeWidth="16" className="pointer-events-auto cursor-pointer"
                      onPointerDown={(e) => { e.stopPropagation(); setSelected({ kind: 'edge', id: ed.id }) }} />
                    <path d={d} fill="none" stroke={on ? 'var(--primary)' : repl ? 'var(--success)' : 'var(--muted)'} strokeWidth={on ? 3 : 2}
                      strokeDasharray={repl ? '7 4' : undefined}
                      markerEnd="url(#stk-arrow)" markerStart={ed.type === 'bidir' ? 'url(#stk-arrow)' : undefined} />
                    {caption && (
                      <text x={(p0.x + p1.x) / 2} y={(p0.y + p1.y) / 2 - 5} textAnchor="middle"
                        style={{ fill: 'var(--muted)', fontSize: '9px', paintOrder: 'stroke', stroke: 'var(--bg)', strokeWidth: 3.5, strokeLinejoin: 'round' }}>
                        {caption}
                      </text>
                    )}
                  </g>
                )
              })}
              {connect && (
                <path d={edgePath(connect.from, 'right', connect.to, 'left')} fill="none" stroke="var(--primary)" strokeWidth="2" strokeDasharray="6 5" />
              )}
            </svg>

            {/* Cluster frames (PXC / ProxySQL), rendered behind nodes with their members */}
            {frames.map((f) => {
              const fdef = NODE_TYPES[f.type] || {}
              const on = selected?.kind === 'frame' && selected.id === f.id
              const kids = nodes.filter((n) => n.frameId === f.id)
              const col = frameColor(f)
              return (
                <div key={f.id} className="group absolute" style={{ left: f.x, top: f.y, width: f.w, height: f.h }}>
                  <div className="absolute inset-0 rounded-xl border-2 border-dashed"
                    style={{ borderColor: on ? 'var(--primary)' : col, background: `color-mix(in srgb, ${col} 7%, transparent)` }} />
                  <div
                    onPointerDown={(e) => startFrame(e, f.id)}
                    onContextMenu={(e) => { e.preventDefault(); e.stopPropagation(); setSelected({ kind: 'frame', id: f.id }) }}
                    className="absolute inset-x-0 top-0 flex cursor-grab items-center gap-2 rounded-t-xl px-2 active:cursor-grabbing"
                    style={{ height: FRAME_TITLE, background: `color-mix(in srgb, ${col} 18%, transparent)` }}
                  >
                    <span style={{ color: col }}>{(Icon[fdef.icon] || Icon.Database)({ size: 15 })}</span>
                    <div className="min-w-0 flex-1 leading-tight">
                      <div className="truncate text-xs font-semibold text-fg">{f.label}</div>
                      <div className="truncate text-[10px] text-muted">{frameDeployedLabel(f, kids, depByNode) || frameVersionLabel(f)} · {kids.length} node{kids.length === 1 ? '' : 's'}</div>
                    </div>
                    {/* PS MongoDB has a fixed topology — no add/remove controls. */}
                    {f.type !== 'psmdb' && (
                      <div className="ml-auto flex items-center gap-0.5">
                        <button title={deploying ? 'Locked while deploying' : 'Add node'} disabled={deploying} onPointerDown={(e) => e.stopPropagation()} onClick={() => addFrameMember(f)}
                          className="rounded px-1.5 text-sm leading-none text-muted hover:bg-surface hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent">+</button>
                        <button title={deploying ? 'Locked while deploying' : 'Remove a node'} disabled={deploying} onPointerDown={(e) => e.stopPropagation()} onClick={() => removePXCNode(f.id)}
                          className="rounded px-1.5 text-sm leading-none text-muted hover:bg-surface hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent">−</button>
                      </div>
                    )}
                  </div>
                  {kids.map((n) => {
                    const non = selected?.kind === 'node' && selected.id === n.id
                    const dep = depByNode[n.id]
                    const arb = n.role === 'arbitrator'
                    const isPrimary = n.role === 'primary'
                    const sub = frameMemberSub(f, n, kids)
                    // Replicas are greyed so a read-only member reads as subordinate.
                    const barCol = (f.type === 'pxc' && arb) || (REPL_FRAME_TYPES.has(f.type) && !isPrimary) ? '#64748b' : col
                    // PXC and Percona Server replication members expose ports for
                    // cross-cluster replication links (the wrapper, not the clipped
                    // card, carries them so they sit outside the rounded border).
                    const canRepl = f.type === 'pxc' || f.type === 'mysql'
                    return (
                      <div key={n.id} className="group absolute"
                        style={{ left: n.x - f.x, top: n.y - f.y, width: PXC_NODE_W, height: PXC_NODE_H }}>
                        <div
                          onPointerDown={(e) => selectFrameNode(e, n.id)}
                          onContextMenu={(e) => openMenu(e, n.id)}
                          onDragOver={(e) => nodeDragOver(e, n.id)}
                          onDragLeave={(e) => nodeDragLeave(e, n.id)}
                          onDrop={(e) => nodeDrop(e, n.id)}
                          className={`absolute inset-0 flex cursor-pointer flex-col overflow-hidden rounded-lg border bg-surface shadow-sm ${non ? 'ring-2 ring-primary' : ''} ${dropNode === n.id ? 'ring-2 ring-success' : ''}`}
                        >
                          <div className="h-1 w-full shrink-0" style={{ background: barCol }} />
                          <div className="flex flex-1 flex-col justify-center px-2 py-1">
                            <div className="flex items-center gap-1">
                              <span className="min-w-0 flex-1 truncate text-xs font-semibold text-fg">{n.label}</span>
                              {dep?.state === 'provisioning' ? (
                                <ProgressRing percent={dep.progress?.percent || 0} size={15} />
                              ) : dep ? (
                                <span className="h-2 w-2 shrink-0 rounded-full" title={dep.state}
                                  style={{ background: `var(--${DEPLOY_TONE[dep.state] === 'success' ? 'success' : dep.state === 'error' ? 'danger' : 'warning'})` }} />
                              ) : null}
                            </div>
                            <div className="mt-0.5 truncate text-[10px] text-muted">{sub}</div>
                            <div className="truncate text-[9px] font-medium text-fg/80">
                              {deployedLabel(n.type, dep) || (f.type === 'k3d' ? 'rancher/k3s' : `${pxcOSLabel(f)} · ${f.arch || platform}`)}
                            </div>
                            {n.exportEnabled && <div className="text-[9px] font-medium text-primary">⇅ export</div>}
                          </div>
                        </div>
                        {canRepl && (
                          <PortHandles ownerId={n.id} connecting={!!connect} snapPort={connect?.targetId === n.id ? connect.targetPort : null} onStart={startConnect} />
                        )}
                      </div>
                    )
                  })}
                  {/* Association endpoints. Driven by the same set hitPort uses:
                      these two disagreed, so repmgr and Valkey cluster frames
                      were droppable but drew no handles — you could finish a
                      line onto them but never start one, and nothing on screen
                      said they were connectable at all. */}
                  {CONNECTABLE_FRAMES.has(f.type) && (
                    <PortHandles ownerId={f.id} connecting={!!connect} snapPort={connect?.targetId === f.id ? connect.targetPort : null} onStart={startConnect} />
                  )}
                </div>
              )
            })}

            {nodes.filter((n) => !n.frameId).map((n) => {
              const def = NODE_TYPES[n.type] || NODE_TYPES.intranet
              const on = selected?.kind === 'node' && selected.id === n.id
              return (
                <div
                  key={n.id}
                  onPointerDown={(e) => startNode(e, n.id)}
                  onContextMenu={(e) => openMenu(e, n.id)}
                  onDragOver={(e) => nodeDragOver(e, n.id)}
                  onDragLeave={(e) => nodeDragLeave(e, n.id)}
                  onDrop={(e) => nodeDrop(e, n.id)}
                  className={`group absolute flex cursor-grab flex-col overflow-hidden rounded-xl border bg-surface shadow-sm active:cursor-grabbing ${on ? 'ring-2 ring-primary' : ''} ${dropNode === n.id ? 'ring-2 ring-success' : ''}`}
                  style={{ left: n.x, top: n.y, width: NODE_W, height: NODE_H }}
                >
                  <div className="h-1.5 w-full shrink-0" style={{ background: def.color }} />
                  <div className="flex flex-1 flex-col justify-center px-3 py-2">
                    <div className="flex items-start gap-2.5">
                      <span className="mt-0.5 shrink-0" style={{ color: def.color }}>
                        {(Icon[def.icon] || Icon.Server)({ size: 22 })}
                      </span>
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2">
                          <div className="min-w-0 flex-1 truncate text-sm font-semibold text-fg">{n.label}</div>
                          <span className="shrink-0">
                            {depByNode[n.id]?.state === 'provisioning' ? (
                              <ProgressRing percent={depByNode[n.id].progress?.percent || 0} size={20} />
                            ) : depByNode[n.id] ? (
                              <Badge tone={DEPLOY_TONE[depByNode[n.id].state] || 'muted'}>{depByNode[n.id].state}</Badge>
                            ) : null}
                          </span>
                        </div>
                        {/* Once deployed, the engine + the version it actually installed with
                            replaces the design-time blurb — that is what an operator looks for. */}
                        <div className="mt-0.5 text-[11px] leading-snug text-muted">
                          {deployedLabel(n.type, depByNode[n.id]) || def.sub}
                        </div>
                        <div className="mt-1 text-[11px] font-medium text-fg/80">{nodeOSLabel(n)}{(n.arch || platform) ? ` · ${n.arch || platform}` : ''}</div>
                      </div>
                    </div>
                  </div>
                  {def.ports && (
                    <PortHandles ownerId={n.id} connecting={!!connect} snapPort={connect?.targetId === n.id ? connect.targetPort : null} onStart={startConnect} />
                  )}
                </div>
              )
            })}
          </div>

          {/* The legend swaps to the drop hint while files are being dragged over
              the canvas — saying it permanently pushes the line under the minimap. */}
          <div className="pointer-events-none absolute bottom-3 left-3 rounded-lg border bg-surface/80 px-3 py-2 text-xs text-muted backdrop-blur">
            {fileDrag
              ? 'Drop on a running node to copy the files into it'
              : 'Drag canvas to pan · scroll to zoom · drag a port to connect · right-click for actions'}
          </div>

          {/* Top-centre: the bottom of the canvas already holds the legend on
              the left and the minimap on the right, and they collide there. */}
          {flash && (
            <div className={`pointer-events-none absolute top-3 left-1/2 max-w-[80%] -translate-x-1/2 rounded-lg border px-3 py-2 text-xs backdrop-blur ${flash.tone === 'err' ? 'border-danger/30 bg-danger/15 text-danger' : 'bg-surface/90 text-fg'}`}>
              {flash.text}
            </div>
          )}

          <Minimap nodes={nodes} view={view} setView={setView} wrapRef={wrapRef} selectedId={selected?.kind === 'node' ? selected.id : null} />
        </div>
        </div>
      </div>

      <StackProperties
        selected={selected}
        stackId={stack.id}
        nodes={nodes}
        edges={edges}
        frames={frames}
        depByNode={depByNode}
        patchNode={patchNode}
        patchFrame={patchFrame}
        patchEdge={patchEdge}
        deleteNode={deleteNode}
        deleteEdge={deleteEdge}
        deleteFrame={deleteFrame}
        rebuildMongoCluster={rebuildMongoCluster}
        deployOpen={deployPanel === 'open'}
        deployments={deployments}
        onDeployMinimize={() => setDeployPanel('min')}
      />

      {menu && (
        <ContextMenu menu={menu} onClose={() => setMenu(null)} actions={nodeMenuActions(menu.id)} />
      )}

      {drop && (
        <NodeDropMenu
          drop={drop}
          node={nodes.find((n) => n.id === drop.id)}
          onPick={runUpload}
          onClose={() => setDrop(null)}
        />
      )}

      {xfer && <UploadDialog xfer={xfer} onCancel={cancelUpload} onClose={() => setXfer(null)} />}

      {fileMgr && (
        <FileManager
          stackId={stack.id}
          nodeId={fileMgr.nodeId}
          nodeLabel={fileMgr.label}
          onClose={() => setFileMgr(null)}
        />
      )}

      {configNode && <ConfigModal dep={configNode} onClose={() => setConfigNode(null)} />}

      {linkPrompt && (
        <LinkDirectionModal
          prompt={linkPrompt} nodes={nodes} edges={edges}
          onClose={() => setLinkPrompt(null)}
          onChoose={(fromEnd, toEnd) => { createFlow(fromEnd, toEnd); setLinkPrompt(null) }}
        />
      )}

      {replPrompt && (
        <ReplicationLinkModal
          prompt={replPrompt} nodes={nodes} frames={frames}
          onClose={() => setReplPrompt(null)}
          onChoose={(fromEnd, toEnd, mode) => { createReplEdge(fromEnd, toEnd, mode); setReplPrompt(null) }}
        />
      )}

      {confirmDel && (
        <DeleteConfirmModal
          info={confirmDel}
          onCancel={() => setConfirmDel(null)}
          onConfirm={() => { const fn = confirmDel.onConfirm; setConfirmDel(null); if (fn) fn() }}
        />
      )}

      {deployPanel === 'min' && createPortal(
        <button
          onClick={() => setDeployPanel('open')}
          className="fixed bottom-3 left-3 z-40 flex items-center gap-2 rounded-lg border bg-surface px-3 py-2 text-sm shadow-lg hover:bg-surface2"
        >
          <Icon.Arrow size={16} /> Deployment
          {deployments.some((d) => d.state === 'pending' || d.state === 'provisioning') && (
            <span className="h-2 w-2 animate-pulse rounded-full bg-warning" />
          )}
        </button>,
        document.body,
      )}
    </div>
  )
}

const DEPLOY_KEY = 'dbcanvas-deploy-layout'
function loadDeployLayout() {
  try { return { docked: true, height: 280, float: { x: 120, y: 120, w: 640, h: 360 }, ...JSON.parse(localStorage.getItem(DEPLOY_KEY) || '{}') } }
  catch { return { docked: true, height: 280, float: { x: 120, y: 120, w: 640, h: 360 } } }
}

// When docked (the default) the console lives at the bottom of the rightmost
// Properties column: `inline` renders it as an in-flow flex child of that column;
// if Properties is detached it falls back to a fixed panel pinned to the right
// edge bottom (`columnWidth` wide). Detached, it floats freely via a portal.
function DeploymentConsole({ deployments, nodes, onMinimize, inline = false, columnWidth = 320 }) {
  const [layout, setLayout] = useState(loadDeployLayout)
  const drag = useRef(null)
  useEffect(() => { try { localStorage.setItem(DEPLOY_KEY, JSON.stringify(layout)) } catch { /* */ } }, [layout])

  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'height') setLayout((l) => ({ ...l, height: Math.min(Math.max(160, d.h0 + (d.y0 - e.clientY)), window.innerHeight - 80) }))
      else if (d.kind === 'move') setLayout((l) => ({ ...l, float: { ...l.float, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) } }))
      else if (d.kind === 'wh') setLayout((l) => ({ ...l, float: { ...l.float, w: Math.max(360, d.w0 + (e.clientX - d.x0)), h: Math.max(200, d.h0 + (e.clientY - d.y0)) } }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  const provisioning = deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')
  const failed = deployments.filter((d) => d.state === 'error')
  const done = !provisioning && deployments.length > 0
  const label = (nid) => nodes.find((n) => n.id === nid)?.label || nid

  const detached = !layout.docked
  let style, cls = 'z-40 flex flex-col border bg-surface shadow-2xl'
  if (detached) {
    style = { position: 'fixed', left: layout.float.x, top: layout.float.y, width: layout.float.w, height: layout.float.h }
  } else if (inline) {
    // in-flow child at the bottom of the Properties column
    style = { height: layout.height }
    cls += ' shrink-0 overflow-hidden rounded-xl'
  } else {
    // docked but Properties is detached: pin to the right-column bottom
    style = { position: 'fixed', right: 0, bottom: 0, width: columnWidth, height: layout.height }
    cls += ' overflow-hidden rounded-xl'
  }

  const node = (
    <div className={cls} style={style}>
      {!detached && (
        <div onPointerDown={(e) => { drag.current = { kind: 'height', y0: e.clientY, h0: layout.height } }}
          className="h-1.5 w-full cursor-ns-resize bg-border/60 hover:bg-primary" />
      )}
      <div
        className="flex items-center gap-2 border-b bg-surface2 px-3 py-1.5"
        onPointerDown={detached ? (e) => { if (e.target.closest('button')) return; drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: layout.float.x, fy: layout.float.y } } : undefined}
        style={detached ? { cursor: 'move' } : undefined}
      >
        <span className="text-sm font-semibold">Deployment</span>
        {provisioning ? (
          <Badge tone="warning">provisioning…</Badge>
        ) : done ? (
          failed.length
            ? <Badge tone="danger">completed with errors — {failed.length} of {deployments.length} failed</Badge>
            : <Badge tone="success">deployment complete</Badge>
        ) : null}
        <div className="ml-auto flex items-center gap-1">
          <button title={detached ? 'Dock' : 'Detach'} onClick={() => setLayout((l) => ({ ...l, docked: !l.docked }))}
            className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={14} /></button>
          <button title="Minimize" onClick={onMinimize} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
        </div>
      </div>
      <div className="flex-1 space-y-3 overflow-auto p-3">
        {deployments.length === 0 && <div className="text-sm text-muted">No nodes deployed.</div>}
        {deployments.map((d) => {
          const p = d.progress || {}
          return (
            <div key={d.nodeId} className="rounded-lg border bg-bg p-2">
              <div className="mb-1 flex items-center gap-2 text-sm">
                <span className="font-medium">{label(d.nodeId)}</span>
                <Badge tone={DEPLOY_TONE[d.state] || 'muted'}>{d.state}</Badge>
                <span className="ml-auto text-xs text-muted">{p.phase || ''}</span>
              </div>
              <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface2">
                <div className={`h-full transition-all ${d.state === 'error' ? 'bg-danger' : d.state === 'running' ? 'bg-success' : 'bg-warning'}`} style={{ width: `${p.percent || 0}%` }} />
              </div>
              {p.message && <div className={`mt-1 text-xs ${d.state === 'error' ? 'text-danger' : 'text-muted'}`}>{p.message}</div>}
              {Array.isArray(p.log) && p.log.length > 0 && (
                <pre className="mt-1.5 max-h-32 overflow-auto whitespace-pre-wrap break-all rounded bg-surface2 p-1.5 text-[11px] leading-tight text-muted">{p.log.slice(-12).join('\n')}</pre>
              )}
            </div>
          )
        })}
      </div>
      {detached && (
        <div onPointerDown={(e) => { drag.current = { kind: 'wh', x0: e.clientX, y0: e.clientY, w0: layout.float.w, h0: layout.float.h } }}
          className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize text-muted">
          <svg viewBox="0 0 10 10" className="h-full w-full"><path d="M9 1 L1 9 M9 5 L5 9" stroke="currentColor" fill="none" /></svg>
        </div>
      )}
    </div>
  )

  // Inline docked → render in flow (the column positions it); otherwise (detached
  // float, or docked-while-Properties-detached) it's fixed, so portal to <body>.
  return inline && !detached ? node : createPortal(node, document.body)
}

// DeleteConfirmModal guards deletion of a *deployed* node or cluster, whose containers
// and volumes are torn down in real time (and can't be undone).
function DeleteConfirmModal({ info, onCancel, onConfirm }) {
  const isFrame = info.kind === 'frame'
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onCancel}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">Delete {isFrame ? 'cluster' : 'node'} “{info.label}”?</h3>
        <p className="mb-4 text-xs text-muted">
          {isFrame
            ? <>This cluster has {info.count} deployed node{info.count === 1 ? '' : 's'}. Deleting it will <span className="font-semibold text-danger">permanently remove</span> their containers and volumes.</>
            : <>This node is deployed. Deleting it will <span className="font-semibold text-danger">permanently remove</span> its container and volumes.</>}
          {' '}This can’t be undone.
        </p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onCancel}>Cancel</Button>
          <Button variant="danger" size="sm" onClick={onConfirm}>Delete</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

// LinkDirectionModal asks which way data flows when two ProxySQL nodes are linked.
// The option whose destination already receives a flow is disabled (a ProxySQL
// can only have one incoming flow).
function LinkDirectionModal({ prompt, nodes, edges, onClose, onChoose }) {
  const { e1, e2 } = prompt
  const labelOf = (id) => nodes.find((n) => n.id === id)?.label || 'node'
  const hasIncoming = (id) => edges.some((ed) => ed.to.node === id)
  const l1 = labelOf(e1.node)
  const l2 = labelOf(e2.node)
  const opts = [
    { from: e1, to: e2, label: `${l1} → ${l2}`, disabled: hasIncoming(e2.node), dest: l2 },
    { from: e2, to: e1, label: `${l2} → ${l1}`, disabled: hasIncoming(e1.node), dest: l1 },
  ]
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">Which way does SQL traffic flow?</h3>
        <p className="mb-3 text-xs text-muted">Pick the direction of the association line between these two ProxySQL nodes.</p>
        <div className="space-y-2">
          {opts.map((o, i) => (
            <button key={i} disabled={o.disabled} onClick={() => onChoose(o.from, o.to)}
              className={`flex w-full items-center justify-between rounded-lg border px-3 py-2 text-sm ${o.disabled ? 'cursor-not-allowed opacity-50' : 'hover:border-primary hover:bg-primary/10'}`}>
              <span className="font-mono">{o.label}</span>
              {o.disabled && <span className="text-[11px] text-muted">{o.dest} already receives a flow</span>}
            </button>
          ))}
        </div>
        {opts.every((o) => o.disabled) && (
          <p className="mt-3 rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
            Both nodes already receive a flow — no direction is available.
          </p>
        )}
        <div className="mt-4 flex justify-end">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

function ConfigModal({ dep, onClose }) {
  let cfg = {}
  try { cfg = typeof dep.config === 'string' ? JSON.parse(dep.config) : dep.config || {} } catch { cfg = {} }
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-md rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold">Node profile</h3>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
        <dl className="space-y-1.5 text-sm">
          <Row k="FQDN" v={cfg.fqdn} />
          <Row k="Domain" v={cfg.domain} />
          <Row k="Base DN" v={cfg.baseDN} />
          <Row k="LDAP admin" v={cfg.ldapAdminDN} />
          <Row k="OS / arch" v={cfg.os ? `${cfg.os} · ${cfg.arch || ''}` : ''} />
          <Row k="Network alias" v={cfg.alias} />
          <Row k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} />
        </dl>
        {Array.isArray(cfg.services) && (
          <div className="mt-3">
            <div className="mb-1 text-xs font-medium text-muted">Services</div>
            <div className="flex flex-wrap gap-1">
              {cfg.services.map((s) => <Badge key={s} tone="primary">{s}</Badge>)}
            </div>
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

function Row({ k, v }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-muted">{k}</dt>
      <dd className="truncate font-mono text-xs text-fg">{v || '—'}</dd>
    </div>
  )
}

// PMMOptions renders the PMM-only node settings: minor-version picker (from the
// catalog produced by `make versions`), admin password (auto-generated when
// empty), and the Intranet-CA certificate toggle.
function PMMOptions({ n, nodes = [], patchNode, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.pmmCatalog().then((c) => { if (alive) setCat(c) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const versions = cat?.versions || []
  const defaultTag = cat?.defaultTag || '3'
  const watchtowers = nodes.filter((x) => x.type === 'watchtower')
  return (
    <>
      <Field
        label="PMM version"
        hint={deployed ? 'Locked — the node is deployed.' : `Default is the rolling latest (percona/pmm-server:${defaultTag}). Pick a minor version to pin it.`}
      >
        <select
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.version || ''}
          disabled={deployed}
          onChange={(e) => patchNode(n.id, { version: e.target.value })}
        >
          <option value="">latest ({defaultTag})</option>
          {versions.map((v) => (
            <option key={v} value={v}>{v}</option>
          ))}
        </select>
      </Field>
      <Field
        label="Admin password"
        hint={deployed ? 'Set at deploy time.' : 'Leave empty to use PMM_ADMIN_PASSWORD from .env.'}
      >
        <input
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.adminPassword || ''}
          disabled={deployed}
          placeholder="(PMM_ADMIN_PASSWORD from .env)"
          onChange={(e) => patchNode(n.id, { adminPassword: e.target.value })}
        />
      </Field>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input
          type="checkbox"
          checked={!!n.generateCert}
          disabled={deployed}
          onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })}
        />
        <span>Generate nginx certificate from Intranet CA</span>
      </label>
      {n.generateCert && !deployed && (
        <p className="text-xs text-muted">
          Requires an Intranet node in the stack. New certs are written to <span className="font-mono">/srv/nginx</span> at deploy.
        </p>
      )}
      <Field
        label="Watchtower"
        hint={deployed ? 'Set at deploy time.' : watchtowers.length ? 'Associate a Watchtower so PMM can perform in-app server upgrades.' : 'Add a Watchtower node to enable in-app upgrades.'}
      >
        <select
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.watchtowerNodeId || ''}
          disabled={deployed || watchtowers.length === 0}
          onChange={(e) => patchNode(n.id, { watchtowerNodeId: e.target.value })}
        >
          <option value="">none</option>
          {watchtowers.map((w) => (
            <option key={w.id} value={w.id}>{w.label}</option>
          ))}
        </select>
      </Field>
      <DirectoryAuthFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} kerberos={false} />
      <KeycloakOidcFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} label="Single sign-on with Keycloak (Grafana OAuth)" />
    </>
  )
}

// ------------------------------------------------------------- PXC cluster forms

// PXCFrameForm edits a PXC cluster frame: version/OS/platform, credentials,
// monitoring/proxy/GTID/TLS options, and shows quorum guidance.
function PXCFrameForm({ frame: f, stackId, nodes, frameNodes, patchFrame, deleteFrame, deployed, running }) {
  const [cat, setCat] = useState(null)
  const [monBusy, setMonBusy] = useState(false)
  const [monMsg, setMonMsg] = useState('')
  const [monErr, setMonErr] = useState('')
  useEffect(() => {
    let alive = true
    stackApi.pxcCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pxcMajor]) || []

  // Cascade-normalize the selection: when OS (or a higher-level field) changes, the
  // dependent fields may become invalid for the new OS (e.g. osVersion stays "9"
  // under ubuntu), leaving major/minor empty. Snap each invalid field to the first
  // valid option for the current catalog, in one pass, until everything is valid.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.pxcMajor) ? f.pxcMajor : (majorList[0] ?? f.pxcMajor)
    if (major !== f.pxcMajor) patch.pxcMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.pxcVersion && !minorList.includes(f.pxcVersion)) patch.pxcVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.pxcMajor, f.pxcVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const regulars = frameNodes.filter((n) => n.role !== 'arbitrator').length
  const total = frameNodes.length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PXC Cluster</span>
        <Badge tone="primary">{total} node{total === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PXC major">
          <select className={`${inputCls} ${lock}`} value={f.pxcMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pxcMajor: e.target.value, pxcVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PXC minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pxcVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pxcVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint={running ? 'Pick a PMM node (or none), then apply to the running cluster.' : 'Optional — registers the cluster with a PMM node.'}>
        <select className={inputCls} value={f.pmmNodeId || ''} onChange={(e) => { patchFrame(f.id, { pmmNodeId: e.target.value }); setMonMsg(''); setMonErr('') }}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>
      {running && (
        <div className="space-y-1.5 rounded-lg border border-dashed p-2">
          <div className="text-xs text-muted">Applies PMM monitoring to the running data nodes now (installs pmm-client and registers, or deregisters when set to none).</div>
          {monErr && <div className="rounded border border-danger/30 bg-danger/15 px-2 py-1 text-xs text-danger">{monErr}</div>}
          {monMsg && <div className="rounded border border-success/30 bg-success/15 px-2 py-1 text-xs text-success">{monMsg}</div>}
          <Button size="sm" className="w-full" disabled={monBusy}
            onClick={async () => {
              setMonBusy(true); setMonErr(''); setMonMsg('')
              try {
                const r = await frameApi(stackId, f.id).setMonitoring(f.pmmNodeId || '')
                setMonMsg(f.pmmNodeId ? `Monitoring enabled (${r.updated} node${r.updated === 1 ? '' : 's'}).` : `Monitoring disabled (${r.updated} node${r.updated === 1 ? '' : 's'}).`)
              } catch (e) { setMonErr(e.message) } finally { setMonBusy(false) }
            }}>
            {monBusy ? 'Applying…' : (f.pmmNodeId ? 'Apply PMM monitoring' : 'Disable PMM monitoring')}
          </Button>
        </div>
      )}

      {/* No "Monitored by (Orchestrator)" picker here, unlike the replication frames.
          Orchestrator discovers a replication tree and fails it over; a PXC cluster
          elects its own primary, so it had nothing to manage — see orchestratableFrame
          in app/orchestrator.go, which no longer accepts this frame type. */}

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for egress</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.gtid !== false} onChange={(e) => patchFrame(f.id, { gtid: e.target.checked })} />
        <span>Enable GTID</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(regulars < 3 || total % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {regulars < 3 && <div>For HA, use at least 3 regular nodes ({regulars} now).</div>}
          {total % 2 === 0 && <div>An odd number of nodes keeps quorum on a split network ({total} now).</div>}
        </div>
      )}
      {regulars === 0 && (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">At least one regular (data) node is required.</div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// PXCNodeForm edits a single PXC cluster member: role and host port export.
function PXCNodeForm({ node: n, frame, nodes, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack.">
        <input className={`${inputCls} opacity-70`} value={n.label} readOnly />
      </Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <Field label="Role" hint={deployed ? 'Locked — the node is deployed.' : 'Arbitrator (garbd) votes for quorum but stores no data.'}>
        <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.role || 'regular'} disabled={deployed} onChange={(e) => patchNode(n.id, { role: e.target.value })}>
          <option value="regular">regular (data node)</option>
          <option value="arbitrator">arbitrator (garbd)</option>
        </select>
      </Field>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.exportEnabled} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={inputCls} value={n.exportHostPort || 0}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// MySQLFrameForm edits a MySQL replication frame: catalog-driven OS/version +
// Percona Server major/minor, replication mode, root password, PMM/proxy/GTID/cert.
function MySQLFrameForm({ frame: f, stackId, nodes, frames, edges, patchFrame, deleteFrame, deployed, running }) {
  const [cat, setCat] = useState(null)
  const [orchBusy, setOrchBusy] = useState(false)
  const [orchMsg, setOrchMsg] = useState('')
  const [orchErr, setOrchErr] = useState('')
  useEffect(() => {
    let alive = true
    stackApi.psCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.psMajor]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.psMajor) ? f.psMajor : (majorList[0] ?? f.psMajor)
    if (major !== f.psMajor) patch.psMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.psVersion && !minorList.includes(f.psVersion)) patch.psVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.psMajor, f.psVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const orchestratorNodes = nodes.filter((x) => x.type === 'orchestrator')
  const members = nodes.filter((x) => x.frameId === f.id)
  const primaries = members.filter((x) => x.role === 'primary').length
  const secondaries = members.length - primaries

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Percona Server Replication</span>
        <Badge tone="primary">{members.length} node{members.length === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Percona Server major">
          <select className={`${inputCls} ${lock}`} value={f.psMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { psMajor: e.target.value, psVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Percona Server minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.psVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { psVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Replication mode" hint={deployed ? 'Locked.' : 'Semi-sync waits for a replica ack on commit.'}>
        <select className={`${inputCls} ${lock}`} value={f.replMode || 'async'} disabled={deployed} onChange={(e) => patchFrame(f.id, { replMode: e.target.value })}>
          <option value="async">normal (asynchronous)</option>
          <option value="semisync">semi-synchronous</option>
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each node with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (Orchestrator)" hint={running ? 'Pick an Orchestrator node (or none), then apply to the running cluster.' : 'Optional — seeds topology discovery on an Orchestrator node.'}>
        <select className={inputCls} value={f.orchestratorNodeId || ''} onChange={(e) => { patchFrame(f.id, { orchestratorNodeId: e.target.value }); setOrchMsg(''); setOrchErr('') }}>
          <option value="">none</option>
          {orchestratorNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>
      {running && (
        <div className="space-y-1.5 rounded-lg border border-dashed p-2">
          <div className="text-xs text-muted">Seeds/refreshes topology discovery on the Orchestrator node now (clearing it just stops re-seeding — Orchestrator itself isn't asked to forget the cluster).</div>
          {orchErr && <div className="rounded border border-danger/30 bg-danger/15 px-2 py-1 text-xs text-danger">{orchErr}</div>}
          {orchMsg && <div className="rounded border border-success/30 bg-success/15 px-2 py-1 text-xs text-success">{orchMsg}</div>}
          <Button size="sm" className="w-full" disabled={orchBusy}
            onClick={async () => {
              setOrchBusy(true); setOrchErr(''); setOrchMsg('')
              try {
                const r = await frameApi(stackId, f.id).setOrchestrator(f.orchestratorNodeId || '')
                setOrchMsg(f.orchestratorNodeId ? `Discovery seeded (${r.updated} node${r.updated === 1 ? '' : 's'}).` : `Link cleared (${r.updated} node${r.updated === 1 ? '' : 's'}).`)
              } catch (e) { setOrchErr(e.message) } finally { setOrchBusy(false) }
            }}>
            {orchBusy ? 'Applying…' : (f.orchestratorNodeId ? 'Apply Orchestrator discovery' : 'Clear Orchestrator link')}
          </Button>
        </div>
      )}

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.gtid !== false} disabled={deployed} onChange={(e) => patchFrame(f.id, { gtid: e.target.checked })} />
        <span>Enable GTID (required for auto-positioning)</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(primaries !== 1 || secondaries === 0) && (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          {primaries !== 1 && <div>Exactly one node must be the primary ({primaries} now).</div>}
          {secondaries === 0 && <div>At least one secondary is required.</div>}
        </div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// MySQLMemberForm edits a MySQL replication member: its role (choosing primary
// auto-demotes the current primary) and host-port export.
function MySQLMemberForm({ node: n, frame, nodes, patchNode, dep, deployed }) {
  const setRole = (role) => {
    if (role === 'primary') {
      // Exactly one primary: demote any other primary in this frame.
      for (const m of nodes) {
        if (m.frameId === n.frameId && m.id !== n.id && m.role === 'primary') patchNode(m.id, { role: 'secondary' })
      }
    }
    patchNode(n.id, { role })
  }
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <Field label="Role" hint={deployed ? 'Locked — the node is deployed.' : 'There is always exactly one primary; the rest are read-only secondaries.'}>
        <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.role || 'secondary'} disabled={deployed} onChange={(e) => setRole(e.target.value)}>
          <option value="primary">primary (read/write)</option>
          <option value="secondary">secondary (read-only)</option>
        </select>
      </Field>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port (3306) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// DirectoryAuthFields renders the "Directory authentication" design block shared by the
// standalone Percona Server / PostgreSQL / PSMDB forms: an LDAP toggle, a directory picker
// (Intranet OpenLDAP or Samba AD DC nodes in the stack), and — when kerberos is allowed and
// a Samba directory is chosen — a Kerberos (GSSAPI) toggle. `ldapBlocked` / `kerberosBlocked`
// (messages) grey out the matching toggle when another feature on the node rules it out.
function DirectoryAuthFields({ node: n, nodes, patchNode, deployed, kerberos, ldapBlocked, kerberosBlocked }) {
  const dirs = nodes.filter((x) => x.type === 'intranet' || x.type === 'sambaad')
  const hasSamba = nodes.some((x) => x.type === 'sambaad')
  const noLdap = deployed || dirs.length === 0 || !!ldapBlocked
  const noKerberos = deployed || !hasSamba || !!kerberosBlocked
  return (
    <div className="space-y-2 rounded-lg border border-dashed p-2">
      <div className="text-xs font-medium text-muted">Directory authentication</div>
      {/* LDAP — against a chosen Intranet or Samba directory */}
      <label className={`flex items-center gap-2 text-sm ${noLdap ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.ldapAuth} disabled={noLdap}
          onChange={(e) => patchNode(n.id, { ldapAuth: e.target.checked })} />
        <span>Integrate with LDAP</span>
      </label>
      {ldapBlocked && <p className="text-xs text-muted">{ldapBlocked}</p>}
      {dirs.length === 0 && <p className="text-xs text-muted">Add an Intranet or Samba AD DC node to enable LDAP login.</p>}
      {n.ldapAuth && dirs.length > 0 && (
        <Field label="Directory">
          <select className={inputCls} value={n.ldapDirNodeId || ''} disabled={deployed}
            onChange={(e) => patchNode(n.id, { ldapDirNodeId: e.target.value })}>
            <option value="">— select —</option>
            {dirs.map((d) => <option key={d.id} value={d.id}>{d.label} ({d.type === 'sambaad' ? 'Samba AD' : 'Intranet LDAP'})</option>)}
          </select>
        </Field>
      )}
      {/* Kerberos — independent of LDAP; requires a Samba AD DC node in the stack */}
      {kerberos && (
        <>
          <label className={`flex items-center gap-2 text-sm ${noKerberos ? 'opacity-70' : ''}`}>
            <input type="checkbox" checked={!!n.kerberosAuth} disabled={noKerberos}
              onChange={(e) => patchNode(n.id, { kerberosAuth: e.target.checked })} />
            <span>Kerberos (GSSAPI) single sign-on</span>
          </label>
          {kerberosBlocked && <p className="text-xs text-muted">{kerberosBlocked}</p>}
          {!hasSamba && <p className="text-xs text-muted">Add a Samba AD DC node to enable Kerberos SSO.</p>}
        </>
      )}
    </div>
  )
}

// VaultFields renders the "Data-at-rest encryption" block shared by the standalone Percona
// Server and PSMDB forms: a toggle + an OpenBao-node picker. How the engine is wired depends on
// its version, which is worth saying out loud here — the keyring_vault *component* only exists
// from Percona Server 8.4; 5.7 and 8.0 use the keyring_vault *plugin*.
// OpenBao is a per-stack singleton, so there is nothing to pick: the toggle links the node to
// the one OpenBao on the canvas (and clears the link when turned off).
function VaultFields({ node: n, nodes, patchNode, deployed }) {
  const bao = nodes.find((x) => x.type === 'openbao')
  const none = deployed || !bao
  const maj = n.psMajor || '8.0'
  const method = n.type === 'psm'
    ? 'mongod security.vault (KV v2)'
    : maj === '8.4'
      ? 'component_keyring_vault (KV v2)'
      : maj === '5.7'
        ? 'keyring_vault plugin (KV v1 — 5.7 predates the v2 API)'
        : 'keyring_vault plugin (KV v2)'
  return (
    <div className="space-y-2 rounded-lg border border-dashed p-2">
      <div className="text-xs font-medium text-muted">Data-at-rest encryption</div>
      <label className={`flex items-center gap-2 text-sm ${none ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.enableVault} disabled={none}
          onChange={(e) => patchNode(n.id, {
            enableVault: e.target.checked,
            openbaoNodeId: e.target.checked ? (bao?.id ?? '') : '',
          })} />
        <span>Encrypt with OpenBao</span>
      </label>
      {!bao && <p className="text-xs text-muted">Add the OpenBao node to key encryption to it.</p>}
      {n.enableVault && bao && (
        <>
          <p className="text-xs text-muted">
            Keyed to <span className="font-mono">{bao.label}</span> (one OpenBao per stack), wired at deploy with
            <span className="font-mono"> {method}</span>. The node gets its own KV mount and a token scoped to it,
            and verifies OpenBao with the Intranet CA it already trusts.
          </p>
          {n.type === 'psm' && (
            <p className="text-xs text-muted">
              MongoDB writes its master key at first start, so encryption is established as the node is deployed —
              it cannot be turned on later without re-creating the data.
            </p>
          )}
        </>
      )}
    </div>
  )
}

// Version pins for KeycloakOidcFields: each engine's OIDC validator exists in exactly one
// series, so turning SSO on moves the node onto it rather than letting validation reject the
// design later. Percona Server's auth_openid_connect plugin arrived in 8.4.11-11, so the
// minor is cleared to "latest" as well — an 8.4 pinned to an older minor has no plugin.
const PG_OIDC_PIN = { patch: { pgMajor: '18', pgVersion: '' }, note: <>Uses PostgreSQL 18 + <span className="font-mono">pg_oidc_validator</span> (set automatically).</> }
const PS_OIDC_PIN = { patch: { psMajor: '8.4', psVersion: '' }, note: <>Uses Percona Server 8.4 (latest minor) + the <span className="font-mono">auth_openid_connect</span> plugin, which Percona added in 8.4.11-11. Not in the 9.7 series yet.</> }

// KeycloakOidcFields renders the shared "Keycloak SSO" design block for the PMM, PostgreSQL
// and Percona Server forms: an enable toggle + a Keycloak-node picker + realm. `pin` (see
// above) moves the node onto the version that engine's OIDC support needs and explains why.
// `blocked` (a message) greys out the toggle when another feature on the node rules OIDC out.
function KeycloakOidcFields({ node: n, nodes, patchNode, deployed, label, pin, blocked }) {
  const kcNodes = nodes.filter((x) => x.type === 'keycloak')
  const sel = kcNodes.find((k) => k.id === n.keycloakNodeId)
  const selSSL = sel ? sel.generateCert !== false : true
  const noOidc = deployed || kcNodes.length === 0 || !!blocked
  return (
    <div className="space-y-2 rounded-lg border border-dashed p-2">
      <div className="text-xs font-medium text-muted">Keycloak SSO</div>
      <label className={`flex items-center gap-2 text-sm ${noOidc ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.enableOIDC} disabled={noOidc}
          onChange={(e) => patchNode(n.id, { enableOIDC: e.target.checked, ...(pin && e.target.checked ? pin.patch : {}) })} />
        <span>{label}</span>
      </label>
      {blocked && <p className="text-xs text-muted">{blocked}</p>}
      {kcNodes.length === 0 && <p className="text-xs text-muted">Add a Keycloak node (with Intranet CA SSL) to enable SSO.</p>}
      {n.enableOIDC && kcNodes.length > 0 && (
        <>
          <Field label="Keycloak node">
            <select className={inputCls} value={n.keycloakNodeId || ''} disabled={deployed}
              onChange={(e) => patchNode(n.id, { keycloakNodeId: e.target.value })}>
              <option value="">— select —</option>
              {kcNodes.map((k) => <option key={k.id} value={k.id}>{k.label}</option>)}
            </select>
          </Field>
          <Field label="Realm" hint="Keycloak realm holding the OIDC client.">
            <input className={inputCls} value={n.oidcRealm ?? 'dbcanvas'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcRealm: e.target.value })} />
          </Field>
          {n.keycloakNodeId && !selSSL && <p className="text-xs text-warning">Enable “Use Intranet CA SSL” on the selected Keycloak — OIDC needs an HTTPS issuer.</p>}
          {pin && <p className="text-xs text-muted">{pin.note}</p>}
        </>
      )}
    </div>
  )
}

// VMSizeFields edits a node's per-node sizing (CPUs + memory in GiB). Both backends honour it:
// Vagrant sizes the VirtualBox VM, Docker passes the values as --cpus/--memory on the container.
// The two differ in what "unset" means, so the fields adapt: Vagrant always sizes a VM and shows
// the 2/2 engine defaults (DBCANVAS_VM_CPUS/MEMORY), while on Docker a blank field means no limit
// — which is what every stack designed before these fields existed keeps getting. Locked once the
// node is deployed, matching the OS/version fields.
//
// The disk rate limits (--device-read-bps/--device-write-bps) are Docker-only, so they appear
// only on that backend. They need a host block device, which the server auto-detects from
// Docker's data root; the override field is revealed once a limit is set, for hosts where that
// detection picks the wrong disk.
function VMSizeFields({ node: n, patchNode, deployed }) {
  const { settings } = useSettings()
  const docker = settings.deploymentBackend !== 'vagrant'
  const lock = deployed ? 'opacity-70' : ''
  // Blank clears the limit (0), so Docker users can go back to unlimited after typing a value.
  const size = (v) => (v === '' ? 0 : Number(v))
  // The device override only matters once a disk limit is actually set.
  const throttled = !!(n.deviceReadMbps || n.deviceWriteMbps)
  return (
    <div className="grid grid-cols-2 gap-2">
      <Field label="CPUs" hint={docker ? 'Container --cpus limit; blank = unlimited.' : 'VirtualBox VM CPUs.'}>
        <input type="number" min="1" max="64" className={`${inputCls} ${lock}`} disabled={deployed}
          placeholder={docker ? 'unlimited' : undefined}
          value={docker ? (n.cpus || '') : (n.cpus || 2)} onChange={(e) => patchNode(n.id, { cpus: size(e.target.value) })} />
      </Field>
      <Field label="Memory (GiB)" hint={docker ? 'Container --memory limit; blank = unlimited.' : 'VirtualBox VM memory.'}>
        <input type="number" min="1" max="256" className={`${inputCls} ${lock}`} disabled={deployed}
          placeholder={docker ? 'unlimited' : undefined}
          value={docker ? (n.memoryGb || '') : (n.memoryGb || 2)} onChange={(e) => patchNode(n.id, { memoryGb: size(e.target.value) })} />
      </Field>
      {docker && (
        <>
          <Field label="Disk read (MB/s)" hint="Container --device-read-bps; blank = unlimited.">
            <input type="number" min="1" max="16384" className={`${inputCls} ${lock}`} disabled={deployed}
              placeholder="unlimited"
              value={n.deviceReadMbps || ''} onChange={(e) => patchNode(n.id, { deviceReadMbps: size(e.target.value) })} />
          </Field>
          <Field label="Disk write (MB/s)" hint="Container --device-write-bps; blank = unlimited.">
            <input type="number" min="1" max="16384" className={`${inputCls} ${lock}`} disabled={deployed}
              placeholder="unlimited"
              value={n.deviceWriteMbps || ''} onChange={(e) => patchNode(n.id, { deviceWriteMbps: size(e.target.value) })} />
          </Field>
          {throttled && (
            <div className="col-span-2 space-y-1">
              <Field label="Block device" hint="Host device the disk limits apply to. Blank = auto-detect the disk backing Docker's data root.">
                <input className={`${inputCls} ${lock}`} disabled={deployed} placeholder="auto-detect (e.g. /dev/sda)"
                  value={n.devicePath ?? ''} onChange={(e) => patchNode(n.id, { devicePath: e.target.value })} />
              </Field>
              <p className="text-xs text-muted">
                Read limits apply only to reads that reach the disk — cached reads are unaffected.
                Write limits also cover buffered writes, but bite at flush, so a workload that never syncs won’t feel them.
              </p>
            </div>
          )}
          <NetworkConditionFields node={n} patchNode={patchNode} />
        </>
      )}
    </div>
  )
}

// NET_SHAPEABLE mirrors netemSupported() in netem.go: the node types that run an
// image carrying tc and have cluster traffic worth impairing. A control that
// silently does nothing is worse than one that is absent, so the section does
// not render for anything else.
const NET_SHAPEABLE = new Set([
  'pxc', 'mariadbgalera', 'ps', 'mysql', 'innodb', 'mysqlce', 'mysqlcerepl',
  'mysqlceinnodb', 'mariadb', 'mariadbrepl', 'patroni', 'pg', 'repmgr', 'spock',
  'psm', 'psmdb', 'psmrs', 'valkey', 'valkeycluster', 'proxysql', 'haproxy',
])

// NetworkConditionFields edits the per-node network impairment: latency, jitter,
// loss and a bandwidth cap, applied with tc after the cluster forms.
//
// Unlike the CPU/memory/disk limits above these are NOT locked once deployed. A
// tc qdisc is a runtime change on a live node, so redeploying a stack re-applies
// them without recreating anything — which is the whole point for a cluster you
// want to break and then repair while watching it.
function NetworkConditionFields({ node: n, patchNode }) {
  const [open, setOpen] = useState(false)
  if (!NET_SHAPEABLE.has(n.type)) return null
  const size = (v) => (v === '' ? 0 : Number(v))
  const on = !!(n.netLatencyMs || n.netJitterMs || n.netLossPct || n.netRateMbit)
  return (
    <details className="col-span-2 rounded-lg border border-border/60 p-2 text-xs" open={open || on}
      onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer text-muted hover:text-fg">
        Network conditions (lab){on && <span className="ml-2 text-status-warn">● impaired</span>}
      </summary>
      <div className="mt-2 space-y-2">
        <p className="text-[11px] leading-relaxed text-muted">
          Degrades this node’s link on purpose, with tc. Applied <em>after</em> the cluster
          forms — a lossy link fails state transfer, so shaping during provisioning would
          break the stack instead of degrading it. Only the node’s database and cluster
          ports are shaped, so DNS and health checks stay clean.
        </p>
        <div className="grid grid-cols-2 gap-2">
          <Field label="Latency (ms)"
            hint="One-way delay added to cluster traffic. This is what drives Galera flow control and, past evs.suspect_timeout, eviction. Max 1000.">
            <input type="number" min="0" max="1000" className={inputCls} placeholder="none"
              value={n.netLatencyMs || ''} onChange={(e) => patchNode(n.id, { netLatencyMs: size(e.target.value) })} />
          </Field>
          <Field label="Jitter (±ms)"
            hint="Spread around that delay, normally distributed. Capped at the latency: a larger jitter reorders packets instead of delaying them, and TCP reads reordering as loss.">
            <input type="number" min="0" max="500" className={inputCls} placeholder="none"
              value={n.netJitterMs || ''} onChange={(e) => patchNode(n.id, { netJitterMs: size(e.target.value) })} />
          </Field>
          <Field label="Packet loss (%)"
            hint="Dropped outbound packets. A few percent is enough to make a synchronous cluster stall; 100% severs the link while leaving the node up, which models a partition rather than a crash.">
            <input type="number" min="0" max="100" step="0.1" className={inputCls} placeholder="none"
              value={n.netLossPct || ''} onChange={(e) => patchNode(n.id, { netLossPct: size(e.target.value) })} />
          </Field>
          <Field label="Bandwidth (Mbit/s)"
            hint="Cap on outbound cluster traffic. Mostly slows state transfer — latency and loss are what actually break a cluster.">
            <input type="number" min="1" max="10000" className={inputCls} placeholder="unlimited"
              value={n.netRateMbit || ''} onChange={(e) => patchNode(n.id, { netRateMbit: size(e.target.value) })} />
          </Field>
        </div>
        <label className="flex items-start gap-2">
          <input type="checkbox" className="mt-0.5" checked={!!n.netAllTraffic}
            onChange={(e) => patchNode(n.id, { netAllTraffic: e.target.checked })} />
          <span className="text-[11px] leading-relaxed text-muted">
            Shape <strong>all</strong> traffic, not just database and cluster ports — models a bad
            NIC rather than a bad link between members. DNS, LDAP and health checks are impaired
            too, so the node may look broken rather than slow.
          </span>
        </label>
      </div>
    </details>
  )
}

// PerconaServerForm edits a standalone Percona Server node: catalog-driven OS/version
// + Percona Server major/minor, root password, PMM/proxy/GTID/cert and host export.
// (Same options as the replication frame, minus the replication mode and role.)
function PerconaServerForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.psMajor]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === n.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(n.psMajor) ? n.psMajor : (majorList[0] ?? n.psMajor)
    if (major !== n.psMajor) patch.psMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (n.psVersion && !minorList.includes(n.psVersion)) patch.psVersion = ''
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.psMajor, n.psVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Percona Server</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Percona Server major">
          <select className={`${inputCls} ${lock}`} value={n.psMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { psMajor: e.target.value, psVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Percona Server minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.psVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { psVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers this server with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={n.gtid !== false} disabled={deployed} onChange={(e) => patchNode(n.id, { gtid: e.target.checked })} />
        <span>Enable GTID</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Generate certificate from Intranet CA</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port (3306) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      <DirectoryAuthFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} kerberos={false} />

      {/* Unlike PostgreSQL, MySQL picks its auth plugin per account, so LDAP and OIDC
          accounts coexist happily on one server — neither blocks the other. */}
      <KeycloakOidcFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed}
        label="Token login with Keycloak (auth_openid_connect)" pin={PS_OIDC_PIN} />

      <VaultFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} />

      {!deployed && <p className="text-xs text-muted">Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// PostgreSQLForm edits a standalone PostgreSQL node: catalog-driven OS/version +
// PostgreSQL major/minor, superuser password, an optional pgBackRest → SeaweedFS S3
// backup (like the Patroni frame), PMM/proxy/cert and host export. A single
// read/write instance — no Patroni/etcd/replication.
function PostgreSQLForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const imgs = usePPGCatalog(n, deployed, patchNode)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const seaweedNodes = nodes.filter((x) => x.type === 'seaweedfs')

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PostgreSQL</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major">
          <select className={`${inputCls} ${lock}`} value={n.pgMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.pgVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.usePgBackRest} disabled={deployed} onChange={(e) => patchNode(n.id, { usePgBackRest: e.target.checked })} />
        <span>Use pgBackRest (SeaweedFS S3) for backup</span>
      </label>
      {n.usePgBackRest && (
        <Field label="SeaweedFS node (S3 repository)" hint={seaweedNodes.length ? 'WAL archive + an initial full backup land here. The node must have S3 TLS enabled (pgBackRest needs HTTPS).' : 'Add a SeaweedFS node (with S3 TLS enabled) to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={n.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}{s.tls ? '' : ' — needs S3 TLS'}</option>)}
          </select>
        </Field>
      )}
      {n.usePgBackRest && (
        <SeaweedBucketField nodes={nodes} nodeId={n.seaweedfsNodeId} value={n.seaweedfsBucket} deployed={deployed}
          onChange={(v) => patchNode(n.id, { seaweedfsBucket: v })} />
      )}

      <Field label="Monitored by (PMM)" hint="Optional — registers this server with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Generate certificate from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port (5432) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {/* LDAP and OIDC are mutually exclusive on PostgreSQL: both need the same pg_hba
          `host all all` catch-all, so only one can ever be live. Kerberos coexists with either. */}
      <DirectoryAuthFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} kerberos={true}
        ldapBlocked={n.enableOIDC ? 'PostgreSQL cannot do LDAP and Keycloak OIDC at once — turn off Keycloak SSO below to use LDAP.' : ''} />

      <KeycloakOidcFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} label="OAuth login with Keycloak (pg_oidc_validator)" pin={PG_OIDC_PIN}
        blocked={n.ldapAuth ? 'PostgreSQL cannot do LDAP and Keycloak OIDC at once — turn off LDAP above to use Keycloak SSO.' : ''} />

      {!deployed && <p className="text-xs text-muted">A single read/write PostgreSQL instance (no replication). Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// SeaweedBucketField lets a backup consumer pick which of a SeaweedFS node's buckets it uses. It
// only appears when there is a choice to make: a node with a single bucket has nothing to pick.
function SeaweedBucketField({ nodes, nodeId, value, onChange, deployed }) {
  const sw = nodes.find((x) => x.id === nodeId && x.type === 'seaweedfs')
  const buckets = sw ? seaweedBucketsOf(sw, false) : []
  if (buckets.length < 2) return null
  return (
    <Field label="Bucket" hint="Which of that node's buckets this cluster backs up to.">
      <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={value || ''} disabled={deployed}
        onChange={(e) => onChange(e.target.value)}>
        <option value="">{buckets[0]} (default)</option>
        {buckets.slice(1).map((b) => <option key={b} value={b}>{b}</option>)}
      </select>
    </Field>
  )
}

// SeaweedFSForm edits a (not-yet-running) SeaweedFS node: the S3 access key
// (AWS_ACCESS_KEY_ID, defaults to "seaweedfs"), the secret key (left empty to
// auto-generate), and the bucket to create. The region is fixed at us-east-1.
function SeaweedFSForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const buckets = seaweedBucketsOf(n, true)
  const badBuckets = buckets.filter((b) => b.trim() && !validBucketName(b))
  const dupBuckets = buckets.filter((b, i) => b.trim() && buckets.findIndex((x) => x.trim() === b.trim()) !== i)
  // The design keeps `bucket` (the first, and what an older design carried) alongside `buckets`, so
  // a stack saved before multi-bucket still deploys and still names its default.
  const writeBuckets = (list) => patchNode(n.id, { buckets: list, bucket: (list[0] || '').trim() })
  const setBucket = (i, v) => writeBuckets(buckets.map((b, j) => (j === i ? v : b)))
  const addBucket = () => writeBuckets([...buckets, ''])
  const removeBucket = (i) => writeBuckets(buckets.filter((_, j) => j !== i))
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">SeaweedFS</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        S3-compatible object storage (<span className="font-mono">chrislusf/seaweedfs</span>),
        used as a backup target for xtrabackup/xbcloud, Percona Backup for MongoDB and pgBackRest.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Field label="AWS_ACCESS_KEY_ID" hint={deployed ? 'Set at deploy.' : 'Defaults to "seaweedfs".'}>
        <input className={`${inputCls} ${lock}`} value={n.accessKey ?? 'seaweedfs'} disabled={deployed} placeholder="seaweedfs"
          onChange={(e) => patchNode(n.id, { accessKey: e.target.value })} />
      </Field>

      <Field label="AWS_SECRET_ACCESS_KEY" hint={deployed ? 'Generated at deploy — see Access tab.' : 'Leave empty to auto-generate.'}>
        <input className={`${inputCls} ${lock}`} value={n.secretKey || ''} disabled={deployed} placeholder="(auto-generate if empty)"
          onChange={(e) => patchNode(n.id, { secretKey: e.target.value })} />
      </Field>

      <Field label="Buckets" hint={`Created at deploy — up to ${MAX_SEAWEED_BUCKETS}. The first one is the default for any node that does not pick.`}>
        <div className="space-y-1">
          {buckets.map((b, i) => (
            <div key={i} className="flex items-center gap-1">
              <input className={`${inputCls} ${lock}`} value={b} disabled={deployed} placeholder="db-backups"
                onChange={(e) => setBucket(i, e.target.value)} />
              {!deployed && buckets.length > 1 && (
                <button title="Remove" onClick={() => removeBucket(i)}
                  className="rounded p-1.5 text-muted hover:bg-surface2 hover:text-danger">
                  <Icon.Trash size={14} />
                </button>
              )}
            </div>
          ))}
        </div>
      </Field>
      {!deployed && buckets.length < MAX_SEAWEED_BUCKETS && (
        <Button variant="outline" size="sm" className="w-full" onClick={addBucket}>
          <Icon.Plus size={14} /> Add bucket
        </Button>
      )}
      {!deployed && badBuckets.length > 0 && (
        <p className="text-xs text-danger">
          Invalid bucket name{badBuckets.length > 1 ? 's' : ''}: {badBuckets.join(', ')} — 3–63 chars, lowercase letters,
          digits, dots and hyphens, starting and ending with a letter or digit.
        </p>
      )}
      {!deployed && dupBuckets.length > 0 && (
        <p className="text-xs text-danger">Duplicate bucket{dupBuckets.length > 1 ? 's' : ''}: {dupBuckets.join(', ')}.</p>
      )}

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        <span className="font-medium text-fg/80">AWS_DEFAULT_REGION</span> is <span className="font-mono">us-east-1</span>.
        The S3 endpoint stays on <span className="font-mono">:8333</span> (used in-network by the database
        nodes); the <span className="font-mono">:8080</span> web interface is published to the host.
      </div>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.tls} disabled={deployed} onChange={(e) => patchNode(n.id, { tls: e.target.checked })} />
        <span>Serve the S3 endpoint over TLS (HTTPS on :8333)</span>
      </label>
      {n.tls && (
        <>
          <label className={`flex items-center gap-2 pl-5 text-sm ${deployed ? 'opacity-70' : ''}`}>
            <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
            <span>Sign the certificate with the Intranet CA</span>
          </label>
          {n.generateCert ? (
            <div className="flex items-center gap-2 pl-5">
              <span className="text-xs text-muted">Cert TTL</span>
              <input type="number" min="1" className={`${inputCls} w-20 ${lock}`} value={n.certTtlValue || 365} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
              <select className={`${inputCls} ${lock}`} value={n.certTtlUnit || 'days'} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
                <option value="minutes">minutes</option>
                <option value="hours">hours</option>
                <option value="days">days</option>
              </select>
            </div>
          ) : (
            <p className="pl-5 text-xs text-muted">Self-signed — clients must skip TLS verification (the snippets set this).</p>
          )}
        </>
      )}

      {!deployed && <p className="text-xs text-muted">The endpoint URL and copy-paste backup snippets appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// WatchtowerForm edits a (not-yet-running) Watchtower node. It is a per-stack
// singleton with no tunables — it runs percona/watchtower with the docker socket
// mounted and its HTTP API enabled so an associated PMM node can drive upgrades.
function WatchtowerForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Watchtower</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Runs <span className="font-mono">percona/watchtower</span> with the Docker socket mounted and its
        HTTP API enabled. Associate it from a PMM node (its options) so PMM can trigger in-app server
        upgrades. One Watchtower per stack.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        Reachable in-network at <span className="font-mono">http://watchtower:8080</span>. A unique HTTP API
        token is generated at deploy and shown here; nothing is published to the host.
      </div>

      {!deployed && <p className="text-xs text-muted">The API token appears here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// WatchtowerManager shows a deployed Watchtower's profile (image, alias, API token).
function WatchtowerManager({ stackId, nodeId, dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Watchtower</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">
        Container auto-upgrades for PMM. Associate it from a PMM node to enable in-app upgrades.
      </p>
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'percona/watchtower:latest'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">API</span><span className="font-mono text-xs">http://{cfg.alias || 'watchtower'}:{cfg.apiPort || 8080}</span></div>
        {sec.apiToken && (
          <div className="flex justify-between gap-3"><span className="text-muted">API token</span><span className="break-all font-mono text-xs">{sec.apiToken}</span></div>
        )}
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// KeycloakForm edits a (not-yet-running) Keycloak node. Per-stack singleton, no
// tunables — it runs the keycloak image in dev mode; a PSMDB node references it to
// enable MONGODB-OIDC. The realm/client/users are set up in the console after deploy.
// OpenBaoForm edits an OpenBao node before deploy. OpenBao installs from EPEL on the systemd
// Oracle Linux 9 image, so the only image choice is the architecture. SSL (an Intranet-CA cert
// staged into /etc/openbao.d/tls) is the default — the Percona engines verify the listener with
// that same CA, and PSMDB refuses a plain-HTTP Vault outside of testing mode.
function OpenBaoForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const ssl = n.generateCert !== false
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">OpenBao</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Vault-compatible secrets manager (<span className="font-mono">dnf install openbao</span> from EPEL),
        used as the KMS for Percona data-at-rest encryption: Percona Server for MySQL via
        <span className="font-mono"> component_keyring_vault</span> (8.4) or the
        <span className="font-mono"> keyring_vault</span> plugin (5.7/8.0), PSMDB via
        <span className="font-mono"> security.vault</span>. Oracle Linux 9 only; one OpenBao per stack.
      </p>

      <Field label="Label" hint="Becomes the node hostname and VAULT_ADDR; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>


      <label className={`flex items-center gap-2 text-sm ${lock}`}>
        <input type="checkbox" checked={ssl} disabled={deployed}
          onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Use Intranet CA SSL (default)</span>
      </label>
      <p className="text-xs text-muted">
        {ssl
          ? 'Serves HTTPS on 8200. The server cert, key and the CA cert go in /etc/openbao.d/tls and are named in /etc/openbao.d/openbao.hcl; VAULT_CACERT points at the CA.'
          : 'HTTP only — PSMDB then needs security.vault.disableTLSForTesting, and MySQL a plain-http vault_url. Enable SSL unless you are testing.'}
      </p>
      {ssl && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} disabled={deployed}
            onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={`${inputCls} ${lock}`} value={n.certTtlUnit || 'days'} disabled={deployed}
            onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${lock}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed}
          onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        At deploy the node is initialized (<span className="font-mono">bao operator init</span>, 5 unseal keys,
        3 to unseal) and unsealed. The keys + root token appear here afterwards — they are shown once by
        OpenBao and nowhere else. KV mounts + policies are created too: <span className="font-mono">mysql-v1</span>,
        <span className="font-mono"> mysql-v2</span> and <span className="font-mono">mongodb-v2</span> (PSMDB
        supports KV v2 only).
      </div>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function KeycloakForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Keycloak</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        OpenID Connect identity provider (<span className="font-mono">quay.io/keycloak/keycloak</span>, dev
        mode). Enable Keycloak OIDC on a PSMDB node to authenticate with it. One Keycloak per stack.
      </p>
      <p className="text-xs text-muted">
        The admin console is not published to this machine — it is served on the stack network only.
        Add an <span className="font-semibold">Ubuntu VNC</span> node (required) and browse to the console
        from its desktop.
      </p>

      <Field label="Label" hint="Becomes the node hostname (also the OIDC issuer host); must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={n.generateCert !== false} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Use Intranet CA SSL (HTTPS issuer)</span>
      </label>
      <p className="text-xs text-muted">
        {n.generateCert !== false
          ? 'Serves HTTPS on 8443 with an Intranet-CA cert; the OIDC issuer is https://<host>:8443. Required for MongoDB OIDC.'
          : 'HTTP only — MongoDB OIDC will not work (it requires an HTTPS issuer). Enable SSL to use it with PSMDB.'}
      </p>
      {n.generateCert !== false && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        Admin console is published to the host on auto-assigned ports (8080 http / 8443 https). The bootstrap
        admin user + password appear here after deploy. When a PSMDB node enables OIDC, its realm, client,
        groups and sample users are created automatically.
      </div>

      {!deployed && <p className="text-xs text-muted">Console URL + admin credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// SambaForm — draft config for the Samba AD DC singleton (Ubuntu 24.04 only).
function SambaForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Samba AD DC</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        An Active Directory Domain Controller — realm from <span className="font-mono">DOMAIN</span>, Administrator
        password from <span className="font-mono">SAMBA_PASSWORD</span>. Manage LDAP users/groups, download
        <span className="font-mono"> krb5.conf</span>, and mint per-service Kerberos principals + keytabs. One per stack.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>
      <Field label="Operating system" hint="Samba AD DC deploys on Ubuntu 24.04 only (complete packages).">
        <input className={`${inputCls} opacity-70`} value="Ubuntu 24.04" readOnly />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Use Intranet CA certificate for LDAPS (TLS)</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        Plain <span className="font-mono">ldap://</span> binds are allowed (<span className="font-mono">ldap server
        require strong auth = no</span>). After deploy, use the LDAP, Kerberos and DB-Auth tabs to manage the
        directory and configure MongoDB / Percona Server / PostgreSQL authentication.
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// KeycloakManager shows a deployed Keycloak's console URL + bootstrap admin creds.
// No host ports are published: because Keycloak issues tokens for its in-network
// FQDN, a forwarded port never gave a working console from the host machine. The
// console is opened from the Ubuntu VNC desktop instead (validateStack requires one).
function KeycloakManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  const consoleURL = cfg.ssl
    ? `https://${cfg.fqdn || cfg.hostname}:8443`
    : `http://${cfg.fqdn || cfg.hostname || 'keycloak'}:8080`
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Keycloak</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">OIDC identity provider. Set up the realm/client/groups/users in the console.</p>
      <div className="rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-xs text-primary">
        The admin console is not published to this machine. Open the{' '}
        <span className="font-semibold">Ubuntu VNC</span> desktop node and browse to{' '}
        <span className="break-all font-mono">{consoleURL}</span> — its browser resolves the stack&apos;s
        DNS names and trusts the Intranet CA.
      </div>
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'quay.io/keycloak/keycloak'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Console</span><span className="break-all font-mono text-xs">{consoleURL}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Issuer base</span><span className="font-mono text-xs">{cfg.ssl ? `https://${cfg.fqdn || cfg.hostname}:8443` : `http://${cfg.hostname || 'keycloak'}:8080`}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">TLS</span><span className="font-mono text-xs">{cfg.ssl ? 'Intranet CA' : 'none (HTTP)'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Admin user</span><span className="font-mono text-xs">{cfg.adminUser || 'admin'}</span></div>
        {sec.adminPassword && (
          <div className="flex justify-between gap-3"><span className="text-muted">Admin password</span><SecretInline value={sec.adminPassword} /></div>
        )}
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// VNCForm edits a (not-yet-running) Ubuntu VNC node: the desktop login user + VNC
// password and whether to route apt through the Intranet proxy.
function VNCForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  // The desktop image is pinned to one Ubuntu release (vncImage in app/vnc.go), so a
  // design saved when the node still had a version picker would deploy on 24.04 while
  // its canvas card claimed 22.04. Snap it, the way the catalog-driven forms snap a
  // version that is no longer offered.
  useEffect(() => {
    if (deployed) return
    if (n.os !== 'ubuntu' || n.osVersion !== '24.04') patchNode(n.id, { os: 'ubuntu', osVersion: '24.04' })
  }, [n.id, n.os, n.osVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Ubuntu VNC</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        XFCE desktop over a browser-based VNC client, with Firefox, the OpenSSH client, the Percona clients
        (MySQL 8.4 with the OpenID Connect plugin, plus PSMDB/Valkey/PostgreSQL), percona-toolkit + ldap-utils
        already in the image (<code>dbcanvas-vnc:ubuntu-24.04</code>, built by <code>make vnc-image</code>).
        The login user has sudo for installing more tools.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Field label="Desktop user" hint="Linux login user (has passwordless sudo).">
        <input className={`${inputCls} ${lock}`} value={n.vncUser ?? 'dbadmin'} disabled={deployed} onChange={(e) => patchNode(n.id, { vncUser: e.target.value })} />
      </Field>

      <Field label="Password" hint={deployed ? 'Set at deploy.' : 'Desktop + VNC password. Empty = VNC_PASSWORD from .env. VNC uses the first 8 characters.'}>
        <input className={`${inputCls} ${lock}`} value={n.vncPassword || ''} disabled={deployed} placeholder="(VNC_PASSWORD from .env)" onChange={(e) => patchNode(n.id, { vncPassword: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      {!deployed && <p className="text-xs text-muted">The web desktop URL + credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// VNCManager shows a deployed VNC node's web desktop URL + credentials.
function VNCManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const url = cfg.webPort ? `http://${host}:${cfg.webPort}/vnc.html` : null
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Ubuntu VNC</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">XFCE desktop with Percona clients. Open the web desktop and enter the VNC password.</p>
      {url && (
        <a href={url} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open web desktop
        </a>
      )}
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'ubuntu:24.04'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        {cfg.webPort ? <div className="flex justify-between gap-3"><span className="text-muted">Web desktop</span><span className="font-mono text-xs">{host}:{cfg.webPort}/vnc.html</span></div> : null}
        <div className="flex justify-between gap-3"><span className="text-muted">Desktop user</span><span className="font-mono text-xs">{cfg.vncUser || 'dbadmin'} (sudo)</span></div>
        {sec.vncPassword && (
          <div className="flex justify-between gap-3"><span className="text-muted">VNC password</span><SecretInline value={sec.vncPassword} /></div>
        )}
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// LinuxClientForm edits a (not-yet-running) Linux Client node: any OS/version/arch from
// the generic images catalog (the same dbcanvas-systemd:* base images every other
// systemd node type uses), an optional package-manager proxy — and nothing else. No
// product gets installed and there's no PMM monitoring; it's a bare jump box for
// reaching the stack's other nodes from its terminal.
function LinuxClientForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.imagesCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]

  // Snap invalid dependent selects once the catalog loads.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Linux Client</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        A bare systemd host with nothing installed — any OS image dbcanvas supports, joined to the
        Intranet DNS/CA. Not monitored by PMM. Install and run whatever clients you need from its terminal.
      </p>

      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// LinuxClientManager shows a deployed Linux Client node's basic connection info — there's
// no service running on it to manage, just the host it joined the stack as.
function LinuxClientManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Linux Client</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">No product installed. Open this node's terminal to install and run clients against the stack.</p>
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || ''}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// SimDashboardLink is the "Open Dashboard" button shown on every first-party sim
// node's Manager, once deployed — its dashboard port is published to the host
// (like PMM's own HTTP/HTTPS ports; see provisionPMM/each sim's provision* for
// the backend half of this), so it opens directly in the host's own browser
// instead of requiring a VNC desktop.
function SimDashboardLink({ port }) {
  if (!port) return null
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const url = `http://${host}:${port}/`
  return (
    <a href={url} target="_blank" rel="noreferrer"
      className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
      <Icon.External size={15} /> Open Dashboard
    </a>
  )
}

// TrafficSimForm edits a (not-yet-running) Traffic Sim node. No OS/version/arch (a
// fixed first-party image) and no config fields — the only thing that matters is
// which Valkey node or Valkey Cluster frame it's linked to, resolved from the
// drawn edge exactly the way the backend's trafficSimTarget does.
function TrafficSimForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const linkedTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const vnode = nodes.find((x) => x.id === other && x.type === 'valkey')
      if (vnode) return { kind: 'valkey', label: vnode.label }
      const vframe = frames.find((x) => x.id === other && x.type === 'valkeycluster')
      if (vframe) return { kind: 'valkeycluster', label: vframe.label }
    }
    return null
  })()

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Traffic Sim</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Background agents simulate a small fictional city's traffic — vehicles, sensors, signals,
        incidents — continuously reading and writing the linked Valkey. A live map is served from
        this node, published to the host (like PMM) once deployed — no VNC desktop needed. No
        product besides the sim itself; not monitored by PMM.
      </p>

      {linkedTarget ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Linked to {linkedTarget.kind === 'valkeycluster' ? 'Valkey Cluster' : 'Valkey'} <span className="font-mono font-medium">{linkedTarget.label}</span>
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a Valkey or Valkey Cluster node to this node.
        </div>
      )}

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// TrafficSimManager shows a deployed Traffic Sim node's published dashboard URL
// and what it's linked to.
function TrafficSimManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Traffic Sim</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <SimDashboardLink port={cfg.httpPort} />
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Internal URL</span><span className="font-mono text-xs">http://{cfg.fqdn || cfg.hostname}:8088</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Linked to</span><span className="font-mono text-xs">{cfg.targetName} ({cfg.targetKind})</span></div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// HotelSimForm edits a (not-yet-running) Hotel Sim node. No OS/version/arch (a
// fixed first-party image) and no config fields — the only thing that matters is
// which PS MongoDB node/frame it's linked to, resolved from the drawn edge exactly
// the way the backend's hotelSimTarget does.
function HotelSimForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const linkedTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const pnode = nodes.find((x) => x.id === other && x.type === 'psm')
      if (pnode) return { kind: 'psm', label: pnode.label }
      const pframe = frames.find((x) => x.id === other && (x.type === 'psmrs' || x.type === 'psmdb'))
      if (pframe) return { kind: pframe.type, label: pframe.label }
    }
    return null
  })()
  const KIND_LABEL = { psm: 'PSMDB', psmrs: 'PSMDB Replica Set', psmdb: 'PSMDB Sharded' }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Hotel Sim</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Background agents run a 100-hotel reservation chain — guest search, booking, modification,
        cancellation, check-in/out — continuously reading and writing the linked MongoDB. A live
        dashboard is served from this node, published to the host (like PMM) once deployed — no
        VNC desktop needed. No product besides the sim itself; not monitored by PMM.
      </p>

      {linkedTarget ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Linked to {KIND_LABEL[linkedTarget.kind]} <span className="font-mono font-medium">{linkedTarget.label}</span>
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a PSMDB, PSMDB Replica Set, or PSMDB Sharded node to this node.
        </div>
      )}

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// HotelSimManager shows a deployed Hotel Sim node's published dashboard URL and
// what it's linked to.
function HotelSimManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const KIND_LABEL = { psm: 'PSMDB', psmrs: 'PSMDB Replica Set', psmdb: 'PSMDB Sharded' }
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Hotel Sim</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <SimDashboardLink port={cfg.httpPort} />
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Internal URL</span><span className="font-mono text-xs">http://{cfg.fqdn || cfg.hostname}:8089</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Linked to</span><span className="font-mono text-xs">{cfg.targetName} ({KIND_LABEL[cfg.targetKind] || cfg.targetKind})</span></div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// AirlineSimForm edits a (not-yet-deployed) Airline Sim node. It resolves its own
// linked target across all five source shapes: a standalone Percona Server node, a
// PXC/MySQL backend frame (direct), an HAProxy node, or a ProxySQL node/cluster —
// the last two only make sense once they're themselves fronting a PXC or MySQL
// replication backend, but that's verified by dbcanvas at deploy time, not here.
function AirlineSimForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const AIRLINE_KIND_LABEL = { ps: 'Percona Server', pxc: 'PXC Cluster', mysql: 'MySQL Replication', haproxy: 'HAProxy', proxysql: 'ProxySQL', 'proxysql-frame': 'ProxySQL Cluster' }
  const linkedTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const psNode = nodes.find((x) => x.id === other && x.type === 'ps' && !x.frameId)
      if (psNode) return { kind: 'ps', label: psNode.label }
      const backendFrame = frames.find((x) => x.id === other && (x.type === 'pxc' || x.type === 'mysql'))
      if (backendFrame) return { kind: backendFrame.type, label: backendFrame.label }
      const haproxyNode = nodes.find((x) => x.id === other && x.type === 'haproxy')
      if (haproxyNode) return { kind: 'haproxy', label: haproxyNode.label }
      const proxysqlNode = nodes.find((x) => x.id === other && x.type === 'proxysql' && !x.frameId)
      if (proxysqlNode) return { kind: 'proxysql', label: proxysqlNode.label }
      const proxysqlFrame = frames.find((x) => x.id === other && x.type === 'proxysql')
      if (proxysqlFrame) return { kind: 'proxysql-frame', label: proxysqlFrame.label }
    }
    return null
  })()

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Airline Sim</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Background agents run a 200-route reservation workload against a 2000-aircraft fleet — route
        search, booking, modification, cancellation, check-in, flight completion — continuously reading
        and writing the linked MySQL-family target. A live dashboard is served from this node,
        published to the host (like PMM) once deployed — no VNC desktop needed. No product besides
        the sim itself; not monitored by PMM.
      </p>

      {linkedTarget ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Linked to {AIRLINE_KIND_LABEL[linkedTarget.kind]} <span className="font-mono font-medium">{linkedTarget.label}</span>
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a standalone Percona Server node, a MySQL
          replication or PXC cluster frame, or a ProxySQL/HAProxy node or cluster fronting one, to this node.
        </div>
      )}

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// AirlineSimManager shows a deployed Airline Sim node's published dashboard URL
// and what it's linked to. cfg.targetKind here is the fully-resolved 7-way kind
// dbcanvas settled on (e.g. "haproxy-pxc"), not the coarser 5-way shape
// AirlineSimForm resolves on the canvas before deploy.
function AirlineSimManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const TARGET_KIND_LABEL = {
    ps: 'Percona Server', mysql: 'MySQL Replication', pxc: 'PXC Cluster',
    'haproxy-pxc': 'HAProxy → PXC', 'haproxy-mysql': 'HAProxy → MySQL Replication',
    'proxysql-pxc': 'ProxySQL → PXC', 'proxysql-mysql': 'ProxySQL → MySQL Replication',
  }
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Airline Sim</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <SimDashboardLink port={cfg.httpPort} />
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Internal URL</span><span className="font-mono text-xs">http://{cfg.fqdn || cfg.hostname}:8090</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Linked to</span><span className="font-mono text-xs">{cfg.targetName} ({TARGET_KIND_LABEL[cfg.targetKind] || cfg.targetKind})</span></div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// CarSimForm edits a (not-yet-deployed) Car Rental Sim node. It resolves its own
// linked target across all five source shapes: a standalone PostgreSQL node, a
// Patroni/repmgr/Spock cluster frame (direct), or an HAProxy node — the last one
// only makes sense once it's itself fronting one of the three cluster kinds, but
// that's verified by dbcanvas at deploy time, not here.
function CarSimForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const CARSIM_KIND_LABEL = { pg: 'PostgreSQL', patroni: 'Patroni Cluster', repmgr: 'repmgr Cluster', spock: 'Spock Cluster', haproxy: 'HAProxy' }
  const linkedTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const pgNode = nodes.find((x) => x.id === other && x.type === 'pg' && !x.frameId)
      if (pgNode) return { kind: 'pg', label: pgNode.label }
      const backendFrame = frames.find((x) => x.id === other && (x.type === 'patroni' || x.type === 'repmgr' || x.type === 'spock'))
      if (backendFrame) return { kind: backendFrame.type, label: backendFrame.label }
      const haproxyNode = nodes.find((x) => x.id === other && x.type === 'haproxy')
      if (haproxyNode) return { kind: 'haproxy', label: haproxyNode.label }
    }
    return null
  })()

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Car Rental Sim</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Background agents run a 180-location rental workload against a 2000-vehicle fleet — location
        search, booking, modification, cancellation, check-out, check-in — continuously reading and
        writing the linked PostgreSQL-family target. A live dashboard is served from this node,
        published to the host (like PMM) once deployed — no VNC desktop needed. No product besides
        the sim itself; not monitored by PMM.
      </p>

      {linkedTarget ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Linked to {CARSIM_KIND_LABEL[linkedTarget.kind]} <span className="font-mono font-medium">{linkedTarget.label}</span>
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a standalone PostgreSQL node, a Patroni/repmgr/Spock
          cluster frame, or an HAProxy node fronting one, to this node.
        </div>
      )}

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// CarSimManager shows a deployed Car Rental Sim node's published dashboard URL
// and what it's linked to. cfg.targetKind here is the fully-resolved 7-way kind
// dbcanvas settled on (e.g. "haproxy-spock"), not the coarser 5-way shape
// CarSimForm resolves on the canvas before deploy.
function CarSimManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const TARGET_KIND_LABEL = {
    pg: 'PostgreSQL', patroni: 'Patroni Cluster', repmgr: 'repmgr Cluster', spock: 'Spock Cluster',
    'haproxy-patroni': 'HAProxy → Patroni', 'haproxy-repmgr': 'HAProxy → repmgr', 'haproxy-spock': 'HAProxy → Spock',
  }
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Car Rental Sim</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <SimDashboardLink port={cfg.httpPort} />
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Internal URL</span><span className="font-mono text-xs">http://{cfg.fqdn || cfg.hostname}:8091</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Linked to</span><span className="font-mono text-xs">{cfg.targetName} ({TARGET_KIND_LABEL[cfg.targetKind] || cfg.targetKind})</span></div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// MarketChaosForm edits a (not-yet-deployed) "Unoptimized MySQL Challenge"
// (MarketChaos) node. It resolves its own linked target across all four source
// shapes: a standalone Percona Server node, a single PXC member node linked
// directly ('replmember' — bypasses cluster-wide resolution on purpose), a
// PXC/MySQL backend frame, or an HAProxy node — the last one only makes sense
// once it's itself fronting one of the former two, but that's verified by
// dbcanvas at deploy time, not here.
// MC_DATASET_PRESETS mirrors marketchaos/internal/sim/dataset.go's own preset
// table — kept as a small display-only copy here (not fetched from the
// engine) since the estimate needs to render before the node is ever
// deployed. medium is the default profile everywhere else in the stack.
const MC_DATASET_PRESETS = {
  small: { label: 'Small', hint: '2K traders, ~575K rows total', size: '~200 MB', time: '~30s' },
  medium: { label: 'Medium (default)', hint: '10K traders, ~5.8M rows total', size: '~1.2 GB', time: '~3-5 min' },
  large: { label: 'Large', hint: '25K traders, ~28M rows total', size: '~5 GB', time: '~15-30 min' },
  custom: { label: 'Custom', hint: 'set exact row counts below', size: null, time: null },
}
const MC_CUSTOM_DEFAULTS = { traders: 10000, orders: 500000, trades: 250000, ticks: 5000000 }

function MarketChaosForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const MARKETCHAOS_KIND_LABEL = { ps: 'Percona Server', pxcnode: 'PXC member (direct)', pxc: 'PXC Cluster', mysql: 'MySQL Replication', haproxy: 'HAProxy' }
  const mcDataset = n.mcDataset || 'medium'
  const preset = MC_DATASET_PRESETS[mcDataset]
  const pxcTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      if (nodes.find((x) => x.id === other && x.type === 'pxc')) return true
      if (frames.find((x) => x.id === other && x.type === 'pxc')) return true
    }
    return false
  })()
  const linkedTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const psNode = nodes.find((x) => x.id === other && x.type === 'ps' && !x.frameId)
      if (psNode) return { kind: 'ps', label: psNode.label }
      const pxcMember = nodes.find((x) => x.id === other && x.type === 'pxc' && x.frameId)
      if (pxcMember) return { kind: 'pxcnode', label: pxcMember.label }
      const backendFrame = frames.find((x) => x.id === other && (x.type === 'pxc' || x.type === 'mysql'))
      if (backendFrame) return { kind: backendFrame.type, label: backendFrame.label }
      const haproxyNode = nodes.find((x) => x.id === other && x.type === 'haproxy')
      if (haproxyNode) return { kind: 'haproxy', label: haproxyNode.label }
    }
    return null
  })()

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Unoptimized MySQL Challenge</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        MarketChaos: a fictional stock exchange deliberately deployed with bad indexes,
        queries, and transaction patterns. Diagnose and fix them against the linked
        MySQL-family target without breaking correctness. A live dashboard is served from
        this node, published to the host (like PMM) once deployed — no VNC desktop needed.
        No product besides the sim itself; not monitored by PMM.
      </p>

      {linkedTarget ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Linked to {MARKETCHAOS_KIND_LABEL[linkedTarget.kind]} <span className="font-mono font-medium">{linkedTarget.label}</span>
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a standalone Percona Server node, a
          direct PXC member node, a PXC/MySQL replication cluster frame, or an HAProxy node
          fronting one, to this node.
        </div>
      )}

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Field label="Dataset size" hint="Fixed at deploy — reseeding at a different size means deleting and redeploying this node.">
        <select
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          disabled={deployed}
          value={mcDataset}
          onChange={(e) => {
            const v = e.target.value
            if (v === 'custom' && !n.mcTraders) {
              patchNode(n.id, { mcDataset: v, mcTraders: MC_CUSTOM_DEFAULTS.traders, mcOrders: MC_CUSTOM_DEFAULTS.orders, mcTrades: MC_CUSTOM_DEFAULTS.trades, mcTicks: MC_CUSTOM_DEFAULTS.ticks })
            } else {
              patchNode(n.id, { mcDataset: v })
            }
          }}
        >
          {Object.entries(MC_DATASET_PRESETS).map(([k, p]) => <option key={k} value={k}>{p.label}</option>)}
        </select>
      </Field>

      {mcDataset === 'custom' ? (
        <div className="grid grid-cols-2 gap-2">
          <Field label="Traders"><input type="number" min={100} className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} disabled={deployed} value={n.mcTraders || ''} onChange={(e) => patchNode(n.id, { mcTraders: parseInt(e.target.value, 10) || 0 })} /></Field>
          <Field label="Orders"><input type="number" min={1000} className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} disabled={deployed} value={n.mcOrders || ''} onChange={(e) => patchNode(n.id, { mcOrders: parseInt(e.target.value, 10) || 0 })} /></Field>
          <Field label="Trades"><input type="number" min={500} className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} disabled={deployed} value={n.mcTrades || ''} onChange={(e) => patchNode(n.id, { mcTrades: parseInt(e.target.value, 10) || 0 })} /></Field>
          <Field label="Price ticks"><input type="number" min={10000} className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} disabled={deployed} value={n.mcTicks || ''} onChange={(e) => patchNode(n.id, { mcTicks: parseInt(e.target.value, 10) || 0 })} /></Field>
        </div>
      ) : (
        <div className="flex justify-between gap-3 rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
          <span>{preset.hint}</span>
          <span className="whitespace-nowrap font-mono">{preset.size} · {preset.time}</span>
        </div>
      )}
      {pxcTarget && (mcDataset === 'large' || mcDataset === 'custom') && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          Linked to a PXC cluster — certification overhead makes seeding roughly 2-3x slower than the same size against a standalone target. Medium is usually plenty for the PXC-specific challenges.
        </div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// MarketChaosManager shows a deployed MarketChaos node's published dashboard URL
// and what it's linked to. cfg.targetKind here is the fully-resolved 5-way kind
// dbcanvas settled on (e.g. "haproxy-pxc"), not the coarser 4-way shape
// MarketChaosForm resolves on the canvas before deploy.
function MarketChaosManager({ node: n, dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const TARGET_KIND_LABEL = {
    ps: 'Percona Server', pxcnode: 'PXC member (direct)', pxc: 'PXC Cluster', mysql: 'MySQL Replication',
    'haproxy-pxc': 'HAProxy → PXC', 'haproxy-mysql': 'HAProxy → MySQL Replication',
  }
  const mcDataset = n?.mcDataset || 'medium'
  const preset = MC_DATASET_PRESETS[mcDataset]
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Unoptimized MySQL Challenge</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <SimDashboardLink port={cfg.httpPort} />
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Internal URL</span><span className="font-mono text-xs">http://{cfg.fqdn || cfg.hostname}:8092</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Linked to</span><span className="font-mono text-xs">{cfg.targetName} ({TARGET_KIND_LABEL[cfg.targetKind] || cfg.targetKind})</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Dataset</span><span className="font-mono text-xs">{preset.label}{mcDataset === 'custom' ? ` (${n.mcTraders || 0}/${n.mcOrders || 0}/${n.mcTrades || 0}/${n.mcTicks || 0})` : ''}</span></div>
      </div>
      <p className="text-xs text-muted">Seeding progress is shown live on the dashboard itself, not here.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// SS_ENGINES mirrors stockSimImplementedEngines in app/stocksim.go, which in
// turn mirrors store.Implemented() inside the sim image. Offering only what the
// binary implements means a user can never configure a target that would be
// refused at startup. `ready` flips as each engine's store lands.
const SS_ENGINES = [
  { id: 'mysql', label: 'MySQL / Percona Server', port: 3306, ready: true },
  { id: 'postgres', label: 'PostgreSQL', port: 5432, ready: true },
  { id: 'mongodb', label: 'MongoDB', port: 27017, ready: true },
  { id: 'valkey', label: 'Valkey', port: 6379, ready: true },
]
// SS_LINK_TYPES mirrors stockSimTarget in app/stocksim.go: every canvas target
// a Stock Market Sim node can be linked to, node or frame, with the label the
// form shows for it. Keys are node/frame types — which for these are also the
// coarse kinds the backend switches on.
export const SS_LINK_TYPES = {
  // Standalone database nodes.
  ps: 'Percona Server',
  mariadb: 'MariaDB',
  mysqlce: 'MySQL',
  pg: 'PostgreSQL',
  psm: 'PS MongoDB',
  valkey: 'Valkey',
  // Cluster frames.
  pxc: 'PXC cluster',
  mysql: 'MySQL replication',
  innodb: 'InnoDB Cluster / Group Replication',
  mariadbrepl: 'MariaDB replication',
  mariadbgalera: 'MariaDB Galera',
  mysqlcerepl: 'MySQL replication (CE)',
  mysqlceinnodb: 'MySQL InnoDB Cluster (CE)',
  patroni: 'Patroni cluster',
  repmgr: 'repmgr cluster',
  spock: 'Spock cluster',
  k3d: 'Kubernetes operator cluster',
  psmrs: 'PSMDB replica set',
  psmdb: 'PSMDB sharded cluster',
  valkeycluster: 'Valkey cluster',
  // Routers. Their engine depends on the backend, so SS_LINK_ENGINE leaves
  // them out and the form reads the engine off the deployed node instead.
  haproxy: 'HAProxy',
  proxysql: 'ProxySQL',
}

// SS_LINKABLE_KINDS is the same set as endpointKind reports it, for the edge rule.
const SS_LINKABLE_KINDS = new Set(Object.keys(SS_LINK_TYPES))

// CONNECTABLE_FRAMES are the cluster frames that expose connection ports, so a
// line can be drawn to or from the frame itself.
//
// A frame type missing from here has no grab handles at all, which looks
// identical to a rule that refuses the link — the edge simply never starts.
// That is what kept a Stock Market Sim node from reaching any of the MySQL
// frames below: the backend accepted them and endpointKind named them, but
// there was nothing on screen to drag from. The node-side equivalent is the
// `ports` flag in NODE_TYPES, which has to be true for the same reason.
export const CONNECTABLE_FRAMES = new Set([
  'pxc', 'proxysql', 'mysql', 'innodb',
  'mariadbrepl', 'mariadbgalera', 'mysqlcerepl', 'mysqlceinnodb',
  'patroni', 'repmgr', 'spock', 'k3d',
  'valkeycluster', 'psmrs', 'psmdb',
])

// SS_AIO_ENGINE mirrors aioEngineForKind in app/aio_target.go: which All in One
// families are databases this application can drive. The absentees — Valkey,
// the two proxies and Orchestrator — are the same ones every other tool in the
// app declines to treat as a query target.
const SS_AIO_ENGINE = { mysql: 'mysql', postgres: 'postgres', mongodb: 'mongodb' }
// SS_LINK_ENGINE mirrors stockSimEngineForKind in app/stocksim.go. In linked
// mode the engine comes from the target at the other end of the line, not from
// the ssEngine field, which only manual mode fills in.
//
// The two routers and the Kubernetes frame are absent for the same reason they
// are absent there: their engine is a property of something else. ProxySQL is
// always MySQL-family, but HAProxy fronts either family, and working out which
// from the canvas is the backend's job — the form treats an unresolved engine
// as "unknown" and simply keeps the size field, which is the harmless
// direction to be wrong in. A K3D frame resolves through SS_K3D_ENGINE below.
export const SS_LINK_ENGINE = {
  ps: 'mysql', mariadb: 'mysql', mysqlce: 'mysql',
  pxc: 'mysql', mysql: 'mysql', innodb: 'mysql',
  mariadbrepl: 'mysql', mariadbgalera: 'mysql',
  mysqlcerepl: 'mysql', mysqlceinnodb: 'mysql',
  proxysql: 'mysql',
  pg: 'postgres', patroni: 'postgres', repmgr: 'postgres', spock: 'postgres',
  psm: 'mongodb', psmrs: 'mongodb', psmdb: 'mongodb',
  valkey: 'valkey', valkeycluster: 'valkey',
}

// SS_K3D_ENGINE mirrors k3dOperatorEngine in app/stocksim_k3d.go: a Kubernetes
// frame's engine is whichever of the six operators it runs, not the frame type,
// so it is absent from SS_LINK_ENGINE for the same reason the routers are.
// A frame with no operator selected has no database in it to drive.
export const SS_K3D_ENGINE = {
  pxc: 'mysql', ps: 'mysql',
  psmdb: 'mongodb',
  pg: 'postgres', cnpg: 'postgres', pgo: 'postgres',
}

// ssLinkEngine resolves one linked target — as SS_LINK_TYPES describes it, plus
// the operator a Kubernetes frame carries — to the engine the sim will speak.
export function ssLinkEngine(target) {
  if (!target) return ''
  if (target.kind === 'k3d') return SS_K3D_ENGINE[target.operator] || ''
  return SS_LINK_ENGINE[target.kind] || ''
}

// SS_CAN_GROW mirrors stockSimGrowable / store.CanGrowToSize: Valkey's tick
// history is a length-capped stream, so no amount of writing makes it bigger.
const SS_CAN_GROW = (engine) => engine !== 'valkey'

// SS_LAB mirrors each store's Capabilities() in the sim image: which of the
// three deliberate-problem knobs an engine can actually turn. MongoDB gets only
// the collection-count one — its transactions need a replica set this node may
// not be pointed at, and a spilling aggregation is a flag rather than a memory
// limit. Valkey gets none: no snapshot, no table handles, no query planner.
const SS_LAB = (engine) => {
  const sql = engine === 'mysql' || engine === 'postgres'
  return {
    idleTxn: sql,
    extraTables: sql || engine === 'mongodb',
    tempTables: sql,
    lockContention: sql,
    // A collection scan is the same pathology as a table scan, and MongoDB
    // reports it honestly in explain's totalDocsExamined.
    scanQueries: sql || engine === 'mongodb',
    writePressure: sql,
  }
}

// SS_LAB_ANY says whether the lab section has anything to show at all, so an
// engine that supports none of it gets no empty disclosure to open.
const SS_LAB_ANY = (engine) => Object.values(SS_LAB(engine)).some(Boolean)

// fmtTargetBytes mirrors stockSimFormatSize, for the deployed node's panel.
function fmtTargetBytes(n) {
  if (!n) return "doesn't grow"
  if (n >= 2 ** 40) return (n / 2 ** 40).toFixed(2) + ' TiB'
  if (n >= 2 ** 30) return (n / 2 ** 30).toFixed(2) + ' GiB'
  if (n >= 2 ** 20) return (n / 2 ** 20).toFixed(1) + ' MiB'
  return (n / 2 ** 10).toFixed(0) + ' KiB'
}

// SS_TARGET_KIND_LABEL names the kind a *deployed* node resolved to. That is a
// superset of SS_LINK_TYPES: waitStockSimTarget rewrites a router's kind to
// name the backend it turned out to be fronting, because "HAProxy" alone does
// not tell you what the application is actually talking to.
const SS_TARGET_KIND_LABEL = {
  ...SS_LINK_TYPES,
  // A Kubernetes frame resolves to the operator that built the database, not to
  // "Kubernetes" — which of the six it was is the whole story of what the
  // application is talking to. Plain 'k3d' stays for nodes deployed before the
  // kind carried the operator.
  'k3d-pxc': 'PXC operator (Kubernetes)',
  'k3d-ps': 'Percona Server operator (Kubernetes)',
  'k3d-psmdb': 'PSMDB operator (Kubernetes)',
  'k3d-pg': 'Percona PostgreSQL operator (Kubernetes)',
  'k3d-cnpg': 'CloudNativePG (Kubernetes)',
  'k3d-pgo': 'Crunchy PGO (Kubernetes)',
  'haproxy-pxc': 'HAProxy → PXC cluster',
  'haproxy-mysql': 'HAProxy → MySQL replication',
  'haproxy-patroni': 'HAProxy → Patroni cluster',
  'haproxy-repmgr': 'HAProxy → repmgr cluster',
  'haproxy-spock': 'HAProxy → Spock cluster',
  'proxysql-pxc': 'ProxySQL → PXC cluster',
  'proxysql-mysql': 'ProxySQL → MySQL replication',
  'aio-mysql': 'All in One (MySQL)',
  'aio-postgres': 'All in One (PostgreSQL)',
  'aio-mongodb': 'All in One (MongoDB)',
  'external-mysql': 'External MySQL',
  'external-postgres': 'External PostgreSQL',
  'external-mongodb': 'External MongoDB',
  'external-valkey': 'External Valkey',
}

// StockSimForm edits a (not-yet-running) Stock Market Sim node. It is the only
// app-simulator form with two connection modes: linked to a database node on
// the canvas (resolved from the drawn edge the way every sibling does), or a
// manual connection typed in here — which needs no edge and can reach a
// database outside the stack entirely.
function StockSimForm({ node: n, nodes, frames, edges, stackId, patchNode, deleteNode, dep, deployed }) {
  const mode = n.ssMode === 'manual' || n.ssMode === 'aio' ? n.ssMode : 'linked'
  const engine = n.ssEngine || 'mysql'
  const engineDef = SS_ENGINES.find((e) => e.id === engine) || SS_ENGINES[0]
  const [test, setTest] = useState(null)
  const [testing, setTesting] = useState(false)

  // Mirrors stockSimTarget's edge walk: a standalone database node, a router,
  // or a cluster frame. Nodes are checked before frames, and a node inside a
  // frame is skipped, exactly as the backend does it — otherwise a line drawn
  // to a cluster member would read as a link to a database this app cannot
  // address on its own.
  const linkedTarget = (() => {
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const db = nodes.find((x) => x.id === other && SS_LINK_TYPES[x.type] && !x.frameId)
      if (db) return { kind: db.type, label: db.label }
      const fr = frames.find((x) => x.id === other && SS_LINK_TYPES[x.type])
      // k3dOperator rides along because a Kubernetes frame's engine is the
      // operator's, not the frame type's — see ssLinkEngine.
      if (fr) return { kind: fr.type, label: fr.label, operator: fr.k3dOperator || '' }
    }
    return null
  })()

  // The All in One nodes in this stack, and the instances declared on the
  // chosen one. Both come from the design rather than from a deployment, so
  // the picker works before anything has been provisioned — which is the point,
  // since this form is what gets filled in first. usable mirrors
  // aioEngineForKind: only the three database families can be driven.
  const aioNodes = nodes.filter((x) => x.type === 'aio')
  const aioInstanceChoices = (() => {
    const node = aioNodes.find((x) => x.id === n.ssAIONode)
    return (node?.aioInstances || []).map((i) => ({
      name: i.name,
      kindLabel: aioKindOf(i.kind)?.label || i.kind,
      engine: SS_AIO_ENGINE[aioFamilyOf(i.kind)] || '',
      usable: !!SS_AIO_ENGINE[aioFamilyOf(i.kind)],
    }))
  })()

  // Whichever of the three modes decided it — the engine the sim will actually
  // run against, and so the one the size target has to make sense for. An
  // unlinked node falls back to the manual field so the form does not flicker
  // between states while a line is being drawn.
  const effectiveEngine =
    mode === 'manual' ? engine
      : mode === 'aio' ? (aioInstanceChoices.find((i) => i.name === n.ssAIOInstance)?.engine || engine)
        : (ssLinkEngine(linkedTarget) || engine)

  async function runTest() {
    setTesting(true)
    setTest(null)
    try {
      setTest(await stackApi.stocksimTest(stackId, n.id, {
        engine, host: n.ssHost || '', port: Number(n.ssPort) || 0,
        user: n.ssUser || '', password: n.ssPassword || '',
        database: n.ssDatabase || 'stocksim', tls: n.ssTLS || 'prefer',
        params: n.ssParams || '', dsn: n.ssDSN || '',
      }))
    } catch (err) {
      setTest({ ok: false, message: err.message || String(err) })
    } finally {
      setTesting(false)
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Stock Market Sim</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        A stock exchange application: background agents move prices, place orders and settle
        trades, while you create, edit and delete securities, portfolios and orders from its own
        web interface. It generates a printable report and CSV exports, and can drop everything
        it created when you're done. A live dashboard is served from this node, published to the
        host (like PMM) once deployed — no VNC desktop needed.
      </p>

      <Field label="Database connection" hint="Where this application keeps its data.">
        <div className="flex gap-1.5">
          {[['linked', 'Linked node'], ['aio', 'All in One'], ['manual', 'Manual']].map(([id, label]) => (
            <button key={id} type="button"
              onClick={() => patchNode(n.id, { ssMode: id })}
              className={`flex-1 rounded-lg border px-2.5 py-1.5 text-xs font-medium ${
                mode === id ? 'border-primary bg-primary/10 text-primary' : 'border-border text-muted hover:border-primary/40'}`}>
              {label}
            </button>
          ))}
        </div>
      </Field>

      {mode === 'linked' ? (
        linkedTarget ? (
          linkedTarget.kind === 'k3d' && !SS_K3D_ENGINE[linkedTarget.operator] ? (
            <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
              Linked to Kubernetes frame <span className="font-mono font-medium">{linkedTarget.label}</span>, which
              runs no database operator. Choose one on the frame — PXC, Percona Server, PSMDB, Percona
              PostgreSQL, CloudNativePG or Crunchy PGO — or link this node to a database elsewhere in
              the stack.
            </div>
          ) : (
            <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
              Linked to {linkedTarget.kind === 'k3d'
                ? `${K3D_OPERATOR_LABEL[linkedTarget.operator]} on Kubernetes`
                : SS_LINK_TYPES[linkedTarget.kind]} <span className="font-mono font-medium">{linkedTarget.label}</span>
            </div>
          )
        ) : (
          <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
            Not linked. Draw an association line to this node from any database in the stack — a
            standalone Percona Server, MariaDB, MySQL, PostgreSQL, PS MongoDB or Valkey node, any
            MySQL, PostgreSQL, MongoDB or Valkey cluster, a Kubernetes frame running any of the six
            database operators, or a ProxySQL/HAProxy node fronting one — or switch to a manual
            connection to use a database outside this stack.
          </div>
        )
      ) : mode === 'aio' ? (
        <div className="space-y-2.5 rounded-lg border border-border bg-surface2 p-2.5">
          <p className="text-xs text-muted">
            An All in One node holds many database instances in one container and draws no
            association lines, so its instance is chosen here rather than with a line. The MySQL,
            PostgreSQL and MongoDB instances can be driven; the proxies, Orchestrator and Valkey
            cannot.
          </p>
          {aioNodes.length === 0 ? (
            <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
              There is no All in One node in this stack to point at. Add one, or choose another
              connection mode.
            </div>
          ) : (
            <>
              <Field label="All in One node">
                <select className={inputCls} value={n.ssAIONode || ''}
                  onChange={(e) => patchNode(n.id, { ssAIONode: e.target.value, ssAIOInstance: '' })}>
                  <option value="">Choose a node…</option>
                  {aioNodes.map((x) => <option key={x.id} value={x.id}>{x.label}</option>)}
                </select>
              </Field>
              <Field label="Instance"
                hint="Cluster instances resolve to their write endpoint — the primary, the bootstrap member or the mongos.">
                <select className={inputCls} value={n.ssAIOInstance || ''}
                  onChange={(e) => patchNode(n.id, { ssAIOInstance: e.target.value })}
                  disabled={!n.ssAIONode}>
                  <option value="">Choose an instance…</option>
                  {aioInstanceChoices.map((i) => (
                    <option key={i.name} value={i.name} disabled={!i.usable}>
                      {i.name} — {i.kindLabel}{i.usable ? '' : ' (not a database this app can drive)'}
                    </option>
                  ))}
                </select>
              </Field>
            </>
          )}
        </div>
      ) : (
        <div className="space-y-2.5 rounded-lg border border-border bg-surface2 p-2.5">
          <p className="text-xs text-muted">
            Connects to any database this stack can reach — elsewhere on this host, on your network,
            or a managed instance. Use <span className="font-mono">host.docker.internal</span> for a
            database running on the Docker host itself. dbcanvas can't verify it before you deploy,
            so test it here.
          </p>
          <Field label="Engine">
            <select className={inputCls} value={engine}
              onChange={(e) => patchNode(n.id, { ssEngine: e.target.value })}>
              {SS_ENGINES.map((e) => (
                <option key={e.id} value={e.id} disabled={!e.ready}>
                  {e.label}{e.ready ? '' : ' — not in this build yet'}
                </option>
              ))}
            </select>
          </Field>
          <div className="grid grid-cols-3 gap-2">
            <div className="col-span-2">
              <Field label="Host">
                <input className={inputCls} value={n.ssHost || ''} placeholder="db.example.com"
                  onChange={(e) => patchNode(n.id, { ssHost: e.target.value })} />
              </Field>
            </div>
            <Field label="Port">
              <input className={inputCls} type="number" value={n.ssPort || ''}
                placeholder={String(engineDef.port)}
                onChange={(e) => patchNode(n.id, { ssPort: Number(e.target.value) || 0 })} />
            </Field>
          </div>
          <div className="grid grid-cols-2 gap-2">
            <Field label="User">
              <input className={inputCls} value={n.ssUser || ''}
                onChange={(e) => patchNode(n.id, { ssUser: e.target.value })} />
            </Field>
            <Field label="Password">
              <input className={inputCls} type="password" value={n.ssPassword || ''}
                onChange={(e) => patchNode(n.id, { ssPassword: e.target.value })} />
            </Field>
          </div>
          <div className="grid grid-cols-2 gap-2">
            <Field label="Database" hint="This app's own schema.">
              <input className={inputCls} value={n.ssDatabase || ''} placeholder="stocksim"
                onChange={(e) => patchNode(n.id, { ssDatabase: e.target.value })} />
            </Field>
            <Field label="TLS">
              <select className={inputCls} value={n.ssTLS || 'prefer'}
                onChange={(e) => patchNode(n.id, { ssTLS: e.target.value })}>
                <option value="prefer">Prefer</option>
                <option value="require">Require</option>
                <option value="disable">Disable</option>
              </select>
            </Field>
          </div>
          <Field label="Display name" hint="Optional — shown on the dashboard and report.">
            <input className={inputCls} value={n.ssLabel || ''} placeholder="Production replica"
              onChange={(e) => patchNode(n.id, { ssLabel: e.target.value })} />
          </Field>
          <details className="text-xs">
            <summary className="cursor-pointer text-muted hover:text-fg">Advanced</summary>
            <div className="mt-2 space-y-2">
              <Field label="Extra driver parameters">
                <input className={inputCls} value={n.ssParams || ''} placeholder="readTimeout=30s"
                  onChange={(e) => patchNode(n.id, { ssParams: e.target.value })} />
              </Field>
              <Field label="Full connection string"
                hint="Overrides every field above. For a connection dbcanvas doesn't model.">
                <input className={inputCls} type="password" value={n.ssDSN || ''}
                  onChange={(e) => patchNode(n.id, { ssDSN: e.target.value })} />
              </Field>
            </div>
          </details>

          <Button variant="secondary" size="sm" className="w-full" onClick={runTest} disabled={testing}>
            {testing ? 'Testing…' : 'Test connection'}
          </Button>
          {test && (
            <div className={`rounded-lg border px-2.5 py-1.5 text-xs ${
              test.ok ? 'border-primary/30 bg-primary/10 text-primary' : 'border-danger/30 bg-danger/15 text-danger'}`}>
              {test.message}
            </div>
          )}
        </div>
      )}

      {SS_CAN_GROW(effectiveEngine) ? (
        <>
          <Field label="Dataset size at High load"
            hint="At the High load level the app writes bulk price history until it owns this much, then stops. Blank uses 5Gi; “off” never grows it.">
            <input className={inputCls} value={n.ssTargetSize || ''} placeholder="5Gi"
              onChange={(e) => patchNode(n.id, { ssTargetSize: e.target.value })} />
          </Field>
          {/* A dataset that is only written to is never read back, so the
              database answers every query out of a few hundred kilobytes of hot
              rows and its cache size makes no measurable difference however
              small you set it. The working set is what makes it matter. */}
          <Field label="Working set"
            hint="How much of that dataset is kept under continuous random read. Blank uses 50%; write it as “50%”, “2.5G” or “off”. Set it larger than the target's cache to see cache size in the numbers."
            >
            <input className={inputCls} value={n.ssWorkingSet || ''} placeholder="50%"
              onChange={(e) => patchNode(n.id, { ssWorkingSet: e.target.value })} />
          </Field>
        </>
      ) : (
        <p className="text-xs text-muted">
          No dataset size target or working set: Valkey keeps its tick history in a
          length-capped stream, so writing harder rolls old entries off rather than growing it,
          and there is no cold data to read back. Link this node to MySQL, PostgreSQL or
          MongoDB to drive storage under load.
        </p>
      )}

      {/* The lab knobs. Each is shown only on an engine that can actually do it
          — SS_LAB mirrors each store's Capabilities() — because a control that
          silently does nothing is worse than one that is absent. */}
      {SS_LAB_ANY(effectiveEngine) && (
        <details className="rounded-lg border border-border/60 p-2 text-xs">
          <summary className="cursor-pointer text-muted hover:text-fg">Deliberate problems (lab)</summary>
          <div className="mt-2 space-y-2">
            <p className="text-[11px] leading-relaxed text-muted">
              Each of these makes the target exhibit one condition that is hard to reproduce on
              purpose and easy to hit by accident. They degrade the database they point at —
              which is the point. Leave them off for anything you care about.
            </p>
            {SS_LAB(effectiveEngine).idleTxn && (
              <Field label="Hold an idle transaction"
                hint="Keeps a transaction open with a read snapshot, so purge can't advance and the history list grows. Write it like 30m or 2h; max 24h. Blank is off.">
                <input className={inputCls} value={n.ssIdleTxn || ''} placeholder="off"
                  onChange={(e) => patchNode(n.id, { ssIdleTxn: e.target.value })} />
              </Field>
            )}
            {SS_LAB(effectiveEngine).extraTables && (
              <Field label="Extra tables"
                hint="Creates this many small tables and reads them in rotation, so table_open_cache stops holding the working set. 0 is off; max 5000.">
                <input className={inputCls} type="number" min="0" max="5000"
                  value={n.ssExtraTables || ''} placeholder="0"
                  onChange={(e) => patchNode(n.id, { ssExtraTables: Number(e.target.value) || 0 })} />
              </Field>
            )}
            {SS_LAB(effectiveEngine).tempTables && (
              <Field label="Temporary-table queries"
                hint="Runs an intraday rollup shaped to build a large intermediate result — in memory, or forced to spill to disk.">
                <select className={inputCls} value={n.ssTempTables || 'off'}
                  onChange={(e) => patchNode(n.id, { ssTempTables: e.target.value })}>
                  <option value="off">Off</option>
                  <option value="memory">In memory</option>
                  <option value="disk">Spilled to disk</option>
                </select>
              </Field>
            )}
            {SS_LAB(effectiveEngine).lockContention && (
              <Field label="Lock contention"
                hint="Concurrent writers competing for a handful of rows this app owns. Light makes them queue, so row lock waits appear. Heavy has them take the same rows in opposite orders, so the server detects and breaks real deadlocks.">
                <select className={inputCls} value={n.ssLockContention || 'off'}
                  onChange={(e) => patchNode(n.id, { ssLockContention: e.target.value })}>
                  <option value="off">Off</option>
                  <option value="light">Light — writers queue</option>
                  <option value="heavy">Heavy — real deadlocks</option>
                </select>
              </Field>
            )}
            {SS_LAB(effectiveEngine).scanQueries && (
              <Field label="Scan queries per minute"
                hint="Reads the tick history with a predicate no index can serve, so the server reads every row to return a handful. Cost grows with the size target. 0 is off; max 120.">
                <input className={inputCls} type="number" min="0" max="120"
                  value={n.ssScanQueries || ''} placeholder="0"
                  onChange={(e) => patchNode(n.id, { ssScanQueries: Number(e.target.value) || 0 })} />
              </Field>
            )}
            {SS_LAB(effectiveEngine).writePressure && (
              <Field label="Write pressure"
                hint="Two different write costs. Commits runs many tiny transactions, so every one pays for its own log flush — the cost is fsyncs. Redo rewrites wide rows in bulk, filling the write-ahead log — the cost is checkpoint headroom. Neither grows the dataset.">
                <select className={inputCls} value={n.ssWritePressure || 'off'}
                  onChange={(e) => patchNode(n.id, { ssWritePressure: e.target.value })}>
                  <option value="off">Off</option>
                  <option value="commits">Commits — many tiny transactions</option>
                  <option value="redo">Redo — bulk rewrites</option>
                </select>
              </Field>
            )}
          </div>
        </details>
      )}

      {/* Applies on every engine — even Valkey, where it is still what decides
          how many connections the app opens. */}
      <Field label="Database threads"
        hint="Concurrent workers writing history and reading the working set back, and the size of the connection pool. Blank uses 4; raise it to put more concurrency on the target.">
        <input className={inputCls} type="number" min="1" max="64"
          value={n.ssThreads || ''} placeholder="4"
          onChange={(e) => patchNode(n.id, { ssThreads: Number(e.target.value) || 0 })} />
      </Field>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// StockSimManager shows a deployed Stock Market Sim node's dashboard URL and
// what it is connected to — and is the one node manager that can delete the
// node's *data* as well as the node. Dropping has to happen first: once the
// container is gone its API is unreachable, and the data lives in a database
// dbcanvas may not otherwise have credentials for.
function StockSimManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const [dropping, setDropping] = useState(false)
  const [alsoDrop, setAlsoDrop] = useState(false)
  const [err, setErr] = useState('')
  const db = cfg.database || 'stocksim'

  async function deleteNodeAndMaybeData() {
    setErr('')
    if (alsoDrop) {
      setDropping(true)
      try {
        const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
        const res = await fetch(`http://${host}:${cfg.httpPort}/api/control/drop`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ confirm: db }),
        })
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText)
      } catch (e) {
        setDropping(false)
        setErr(`Could not drop the data: ${e.message}. The node has not been deleted.`)
        return
      }
      setDropping(false)
    }
    onDeleteNode()
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Stock Market Sim</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <SimDashboardLink port={cfg.httpPort} />
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Internal URL</span><span className="font-mono text-xs">http://{cfg.fqdn || cfg.hostname}:8093</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Connected to</span><span className="font-mono text-xs">{cfg.targetName} ({SS_TARGET_KIND_LABEL[cfg.targetKind] || cfg.targetKind})</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Database</span><span className="font-mono text-xs">{cfg.engine} / {db}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Dataset at High</span><span className="font-mono text-xs">{fmtTargetBytes(cfg.targetBytes)}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Working set</span><span className="font-mono text-xs">{cfg.workingSet || '50%'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Threads</span><span className="font-mono text-xs">{cfg.threads || 4}</span></div>
      </div>
      {cfg.httpPort && (
        <a href={`http://${typeof location !== 'undefined' ? location.hostname : 'localhost'}:${cfg.httpPort}/report`}
          target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-border px-3 py-2 text-sm hover:border-primary/40">
          <Icon.External size={15} /> Open report
        </a>
      )}

      <label className="flex items-start gap-2 rounded-lg border border-danger/25 bg-danger/10 px-2.5 py-2 text-xs">
        <input type="checkbox" className="mt-0.5" checked={alsoDrop}
          onChange={(e) => setAlsoDrop(e.target.checked)} />
        <span className="text-muted">
          Also drop this application's tables from <span className="font-mono text-fg">{db}</span> on{' '}
          <span className="font-mono text-fg">{cfg.targetName}</span>. The database itself and anything
          else in it are left alone.
        </span>
      </label>
      {err && <p className="text-xs text-danger">{err}</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={deleteNodeAndMaybeData} disabled={dropping}>
        <Icon.Trash size={16} /> {dropping ? 'Dropping data…' : alsoDrop ? 'Delete node and its data' : 'Delete node'}
      </Button>
    </div>
  )
}

// ValkeyForm edits a (not-yet-running) standalone Valkey node: password (requirepass),
// optional LDAP auth against the Intranet OpenLDAP, PMM monitoring and host-port export.
function ValkeyForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const imgs = useValkeyCatalog(n, deployed, patchNode)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.valkeyMajor]) || []
  const debian = n.os === 'ubuntu' || n.os === 'debian'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Valkey (standalone)</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Installed via percona-release (the <span className="font-mono">valkey-91</span> repo) on a systemd base
        image, like every other Percona product here. pmm-client is installed via percona-release too.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Valkey major">
          <select className={`${inputCls} ${lock}`} value={n.valkeyMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { valkeyMajor: e.target.value, valkeyVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Valkey minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.valkeyVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { valkeyVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useLdap} disabled={deployed || debian} onChange={(e) => patchNode(n.id, { useLdap: e.target.checked })} />
        <span>Enable LDAP auth (Intranet OpenLDAP)</span>
      </label>
      {n.useLdap && !debian && <p className="text-xs text-muted">Wires the valkey-ldap module to <span className="font-mono">ldap://intranet:389</span> (users under <span className="font-mono">ou=People</span>).</p>}
      {debian && <p className="text-xs text-muted">percona-valkey-ldap isn't published for Ubuntu yet — pick Oracle Linux for LDAP auth.</p>}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <Field label="Monitored by (PMM)" hint="Optional — installs/registers pmm-client.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export Valkey port (6379) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {!deployed && <p className="text-xs text-muted">Connection info + password appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ValkeyManager shows a deployed standalone Valkey's connection info + credentials.
function ValkeyManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const isCluster = cfg.role === 'cluster'
  const clusterFlag = isCluster ? '-c ' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">{isCluster ? 'Valkey (cluster member)' : 'Valkey (standalone)'}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Base image</span><span className="font-mono text-xs">{cfg.image || 'dbcanvas-systemd'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">LDAP</span><span className="font-mono text-xs">{cfg.useLdap ? (cfg.ldapServers || 'enabled') : 'disabled'}</span></div>
        {cfg.exportPort ? <div className="flex justify-between gap-3"><span className="text-muted">Exported port</span><span className="font-mono text-xs">{host}:{cfg.exportPort}</span></div> : null}
        <div className="flex justify-between gap-3"><span className="text-muted">Monitored by</span><span className="font-mono text-xs">{cfg.monitoredBy || '—'}</span></div>
        {sec.password && <div className="flex justify-between gap-3"><span className="text-muted">Default password</span><SecretInline value={sec.password} /></div>}
      </div>
      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs space-y-1">
        <div className="text-muted">Connect as the default user ({clusterFlag ? 'cluster mode' : 'direct'}):</div>
        {/* The password is masked in place — the whole command still copies with it. */}
        {cfg.exportPort ? (
          <div className="flex flex-wrap items-center gap-1 font-mono">
            <span>valkey-cli {clusterFlag}-h {host} -p {cfg.exportPort} -a '</span>
            <SecretInline value={sec.password} /><span>'</span>
            <CopyBtn text={`valkey-cli ${clusterFlag}-h ${host} -p ${cfg.exportPort} -a '${sec.password || ''}'`} />
          </div>
        ) : null}
        <div className="flex flex-wrap items-center gap-1 font-mono">
          <span>valkey-cli {clusterFlag}-h {cfg.fqdn} -p 6379 -a '</span>
          <SecretInline value={sec.password} /><span>'</span>
          <CopyBtn text={`valkey-cli ${clusterFlag}-h ${cfg.fqdn} -p 6379 -a '${sec.password || ''}'`} />
          <span className="text-muted">(in-cluster)</span>
        </div>
      </div>
      {cfg.useLdap && (
        <div className="rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 text-xs space-y-1">
          <div className="font-semibold text-primary">LDAP login (Intranet OpenLDAP)</div>
          <div className="text-muted">The LDAP user must first exist as a Valkey ACL user (passwordless — the password is verified against LDAP). As the default user, create it:</div>
          <div className="break-all font-mono">valkey-cli {clusterFlag}-h {cfg.fqdn} -p 6379 -a '{sec.password || ''}' ACL SETUSER alice on '~*' +@all</div>
          <div className="text-muted">Then connect as the LDAP user (uid=alice,ou=People; password from LDAP):</div>
          <div className="break-all font-mono">valkey-cli {clusterFlag}-h {cfg.fqdn} -p 6379 --user alice -a '&lt;ldap-password&gt;'</div>
          <div className="text-muted">From the host use <span className="font-mono">-h {host} -p {cfg.exportPort || '&lt;export-port&gt;'}</span> (enable export to reach it from outside the stack).</div>
        </div>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// The Kubernetes Service types a cr.yaml `expose` section accepts.
const K3D_EXPOSE_OPTIONS = [
  { id: 'clusterip', label: 'ClusterIP (in-cluster only)' },
  { id: 'nodeport', label: 'NodePort' },
  { id: 'loadbalancer', label: 'LoadBalancer (MetalLB)' },
]

// K3DFrameForm edits a K3D cluster frame: size, the CPU/memory budget for the whole cluster, and
// what to install on it. CPU/memory are a *total*, split across the nodes — which is why the hints
// warn in terms of the cluster, not the node.
function K3DFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const count = frameNodes.length
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const swNodes = nodes.filter((x) => x.type === 'seaweedfs')
  const cpus = f.k3dCpus || 4
  // The device override only matters once a disk limit is actually set.
  const k3dThrottled = !!(f.k3dDiskReadMbps || f.k3dDiskWriteMbps)
  const memGb = f.k3dMemoryGb || 8
  const tooSmall = cpus < 4 || memGb < 6

  const [ops, setOps] = useState(null)
  const [k3s, setK3s] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.operatorsCatalog().then((c) => { if (alive) setOps(c || {}) }).catch(() => { /* keep the defaults */ })
    stackApi.k3sCatalog().then((c) => { if (alive) setK3s(c || null) }).catch(() => { /* keep the defaults */ })
    return () => { alive = false }
  }, [])
  const op = f.k3dOperator || ''
  // The two community PostgreSQL operators are Helm-installed, so their version is a *chart*
  // version and each has its own knobs. Crunchy's chart lives in an OCI registry rather than on
  // a Helm HTTP repo, but the catalog treats both the same way.
  const cnpg = op === 'cnpg'
  const pgo = op === 'pgo'
  const helmOp = cnpg || pgo
  // The catalog namespaces charts and chart-selected images apart from the Percona operators,
  // because a chart version and an operator version are different kinds of thing.
  const chartKey = pgo ? 'chart:pgo' : 'chart:cloudnative-pg'
  const chartVersions = ops?.[chartKey]?.versions || []
  const chartLatest = ops?.[chartKey]?.latest || ''
  const pgMajors = ops?.[pgo ? 'image:crunchy-postgres' : 'image:cnpg-postgresql']?.versions || []
  const versions = helmOp ? chartVersions : (ops?.[op]?.versions || [])
  const latest = helmOp ? chartLatest : (ops?.[op]?.latest || '')
  // A sharded MongoDB cluster is 9 pods (replica set + config servers + mongos), not 3 — and so is an
  // async Percona Server cluster (MySQL + Orchestrator + HAProxy).
  const psAsync = op === 'ps' && f.k3dClusterType === 'async'
  // MySQL Router only speaks group replication, so an async cluster is HAProxy either way.
  const psFrontEnd = psAsync ? 'haproxy' : (f.k3dProxy === 'router' ? 'router' : 'haproxy')
  const shardedTooSmall = ((op === 'psmdb' && f.k3dSharding) || psAsync) && (cpus < 8 || memGb < 12)

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">K3D Cluster</span>
        <Badge tone="muted">{count} node{count === 1 ? '' : 's'}</Badge>
      </div>
      <p className="text-xs text-muted">
        A k3s cluster created by <span className="font-mono">k3d</span> on the stack network — so pods reach the
        Intranet DNS, PMM and SeaweedFS like any other node. MetalLB provides LoadBalancer addresses from the
        stack subnet. Use the frame +/- to resize (1–3 nodes; the first is the server).
      </p>

      <Field label="Cluster name" hint="Frame label; becomes the k3d cluster name.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <Field label="Kubernetes (k3s)" hint="The k3s image the nodes run. From `make versions`.">
        <select className={`${inputCls} ${lock}`} value={f.k3dK3sVersion || ''} disabled={deployed}
          onChange={(e) => patchFrame(f.id, { k3dK3sVersion: e.target.value })}>
          <option value="">latest{k3s?.latest ? ` (${k3s.latest})` : ''}</option>
          {(k3s?.versions || []).map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="CPUs (whole cluster)">
          <input type="number" min="1" max="64" className={`${inputCls} ${lock}`} value={cpus} disabled={deployed}
            onChange={(e) => patchFrame(f.id, { k3dCpus: Number(e.target.value) })} />
        </Field>
        <Field label="Memory GiB (whole cluster)">
          <input type="number" min="1" max="256" className={`${inputCls} ${lock}`} value={memGb} disabled={deployed}
            onChange={(e) => patchFrame(f.id, { k3dMemoryGb: Number(e.target.value) })} />
        </Field>
      </div>
      <p className="text-xs text-muted">Split evenly across the {count} node{count === 1 ? '' : 's'} ({Math.max(1, Math.floor(memGb / count))} GiB each).</p>

      <div className="grid grid-cols-2 gap-2">
        <Field label="Disk read (MB/s, per node)" hint="Blank = unlimited.">
          <input type="number" min="1" max="16384" className={`${inputCls} ${lock}`} disabled={deployed}
            placeholder="unlimited" value={f.k3dDiskReadMbps || ''}
            onChange={(e) => patchFrame(f.id, { k3dDiskReadMbps: e.target.value === '' ? 0 : Number(e.target.value) })} />
        </Field>
        <Field label="Disk write (MB/s, per node)" hint="Blank = unlimited.">
          <input type="number" min="1" max="16384" className={`${inputCls} ${lock}`} disabled={deployed}
            placeholder="unlimited" value={f.k3dDiskWriteMbps || ''}
            onChange={(e) => patchFrame(f.id, { k3dDiskWriteMbps: e.target.value === '' ? 0 : Number(e.target.value) })} />
        </Field>
      </div>
      {k3dThrottled && (
        <>
          <Field label="Block device" hint="Host device the disk limits apply to. Blank = auto-detect the disk backing Docker's data root.">
            <input className={`${inputCls} ${lock}`} disabled={deployed} placeholder="auto-detect (e.g. /dev/sda)"
              value={f.k3dDevicePath ?? ''} onChange={(e) => patchFrame(f.id, { k3dDevicePath: e.target.value })} />
          </Field>
          <p className="text-xs text-muted">
            Unlike CPU and memory these are <em>per node</em>, not a cluster total split up — the kernel throttles
            per cgroup, so a shared cluster-wide ceiling isn’t something it can enforce. Applied to each k3s node’s
            cgroup after k3d creates it, which covers the pods running on it.
          </p>
        </>
      )}
      {tooSmall && (
        <p className="text-xs text-amber-500">
          Below 4 CPU / 6 GiB a database cluster (3 pods plus a proxy or router) is unlikely to schedule. Validation
          warns, it does not block.
        </p>
      )}
      {shardedTooSmall && (
        <p className="text-xs text-amber-500">
          {psAsync
            ? 'An async cluster is 9 pods (3 MySQL + 3 Orchestrator + 3 HAProxy). Below 8 CPU / 12 GiB, use group replication instead.'
            : 'A sharded MongoDB cluster is 9 pods (replica set + config servers + mongos). Below 8 CPU / 12 GiB, deploy it as a replica set instead.'}
        </p>
      )}

      <div className="space-y-2 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Database operator</div>
        <Field label="Operator">
          <select className={`${inputCls} ${lock}`} value={op} disabled={deployed}
            onChange={(e) => patchFrame(f.id, {
              k3dOperator: e.target.value, k3dOperatorVer: '',
              k3dNamespace: e.target.value || 'default',
            })}>
            <option value="">none (plain Kubernetes)</option>
            <option value="pxc">Percona Operator for MySQL (PXC)</option>
            <option value="ps">Percona Operator for MySQL (Percona Server)</option>
            <option value="psmdb">Percona Operator for MongoDB (PSMDB)</option>
            <option value="pg">Percona Operator for PostgreSQL (PGO)</option>
            <option value="cnpg">CloudNativePG (PostgreSQL)</option>
            <option value="pgo">Crunchy Postgres for Kubernetes (PGO)</option>
          </select>
        </Field>
        {op && (
          <>
            {helmOp ? (
              <Field label="Chart version" hint={pgo
                ? 'PGO Helm chart version, from `make versions` — the tags Crunchy publishes to their OCI registry. Not the GitHub tags: some of those have no published image.'
                : 'CloudNativePG Helm chart version, from `make versions`. Not the operator version it ships \u2014 chart 0.29.0 carries operator 1.30.x.'}>
                {chartVersions.length ? (
                  <select className={`${inputCls} ${lock}`} value={f.k3dOperatorVer || ''} disabled={deployed}
                    onChange={(e) => patchFrame(f.id, { k3dOperatorVer: e.target.value })}>
                    <option value="">latest{chartLatest ? ` (${chartLatest})` : ''}</option>
                    {chartVersions.map((v) => <option key={v} value={v}>{v}</option>)}
                  </select>
                ) : (
                  /* No catalog yet (`make versions` never run): helm still resolves a blank to latest. */
                  <input className={`${inputCls} ${lock}`} value={f.k3dOperatorVer || ''} disabled={deployed}
                    placeholder="latest — run `make versions` to list them"
                    onChange={(e) => patchFrame(f.id, { k3dOperatorVer: e.target.value })} />
                )}
              </Field>
            ) : (
              <Field label="Operator version" hint="From `make versions`. The source is unpacked into /root on the first node.">
                <select className={`${inputCls} ${lock}`} value={f.k3dOperatorVer || ''} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dOperatorVer: e.target.value })}>
                  <option value="">latest{latest ? ` (${latest})` : ''}</option>
                  {versions.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
              </Field>
            )}
            <Field label="Namespace" hint={cnpg ? 'The Cluster CR is created here; the operator itself runs in cnpg-system.'
              : pgo ? 'The operator and the PostgresCluster both run here.'
                : 'The operator and its cr.yaml are installed here.'}>
              <input className={`${inputCls} ${lock}`} value={f.k3dNamespace ?? op} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dNamespace: e.target.value })} />
            </Field>
          </>
        )}
        {cnpg && (
          <>
            <div className="grid grid-cols-2 gap-2">
              <Field label="Instances" hint="Postgres pods (1 primary + replicas).">
                <input type="number" min="1" max="5" className={`${inputCls} ${lock}`} disabled={deployed}
                  value={f.k3dCnpgInstances || 3}
                  onChange={(e) => patchFrame(f.id, { k3dCnpgInstances: Number(e.target.value) })} />
              </Field>
              <Field label="Storage (GiB per instance)">
                <input type="number" min="1" max="512" className={`${inputCls} ${lock}`} disabled={deployed}
                  value={f.k3dCnpgStorageGb || 1}
                  onChange={(e) => patchFrame(f.id, { k3dCnpgStorageGb: Number(e.target.value) })} />
              </Field>
            </div>
            <Field label="PostgreSQL major" hint="Blank = the operator's default. Pins imageName to ghcr.io/cloudnative-pg/postgresql:<major>.">
              {pgMajors.length ? (
                <select className={`${inputCls} ${lock}`} value={f.k3dCnpgVersion || ''} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dCnpgVersion: e.target.value })}>
                  <option value="">operator default</option>
                  {pgMajors.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
              ) : (
                <input className={`${inputCls} ${lock}`} value={f.k3dCnpgVersion ?? ''} disabled={deployed}
                  placeholder="operator default (e.g. 17)"
                  onChange={(e) => patchFrame(f.id, { k3dCnpgVersion: e.target.value })} />
              )}
            </Field>
            <Field label="Expose · Postgres primary" hint="CloudNativePG's own services are all ClusterIP. A LoadBalancer address makes the primary reachable from outside the cluster, and follows failover.">
              <select className={`${inputCls} ${lock}`} value={f.k3dCnpgExpose || 'clusterip'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dCnpgExpose: e.target.value })}>
                <option value="clusterip">ClusterIP (in-cluster only)</option>
                <option value="loadbalancer">LoadBalancer (MetalLB address)</option>
              </select>
            </Field>
            {/* PgBouncer is a Pooler CR of CloudNativePG's own, with its own Service — so it is a
                separate toggle with a separate expose, not a section of the Cluster. */}
            <div className="space-y-2 rounded-lg border border-dashed p-2">
              <label className="flex items-start gap-2 text-sm">
                <input type="checkbox" className="mt-1" disabled={deployed}
                  checked={!!f.k3dCnpgPooler}
                  onChange={(e) => patchFrame(f.id, { k3dCnpgPooler: e.target.checked })} />
                <span>
                  Connection pooling with PgBouncer
                  <span className="block text-xs text-muted">
                    Adds a <span className="font-mono">Pooler</span> of type <span className="font-mono">rw</span>, so the
                    pool follows the primary across a failover. CloudNativePG wires it to the cluster's own
                    credentials — the app role connects through it with the same password as direct.
                  </span>
                </span>
              </label>
              {f.k3dCnpgPooler && (
                <>
                  <div className="grid grid-cols-2 gap-2">
                    <Field label="PgBouncer pods">
                      <input type="number" min="1" max="5" className={`${inputCls} ${lock}`} disabled={deployed}
                        value={f.k3dCnpgPoolerInstances || 2}
                        onChange={(e) => patchFrame(f.id, { k3dCnpgPoolerInstances: Number(e.target.value) })} />
                    </Field>
                    <Field label="Pool mode" hint="Transaction pooling shares a backend between statements — much better reuse, but the client cannot rely on session state.">
                      <select className={`${inputCls} ${lock}`} value={f.k3dCnpgPoolerMode || 'session'} disabled={deployed}
                        onChange={(e) => patchFrame(f.id, { k3dCnpgPoolerMode: e.target.value })}>
                        <option value="session">session (CNPG default)</option>
                        <option value="transaction">transaction</option>
                      </select>
                    </Field>
                  </div>
                  <Field label="Expose · PgBouncer" hint="Independent of the Postgres setting above — pooling the primary while Postgres itself stays in-cluster is the usual arrangement.">
                    <select className={`${inputCls} ${lock}`} value={f.k3dCnpgPoolerExpose || 'clusterip'} disabled={deployed}
                      onChange={(e) => patchFrame(f.id, { k3dCnpgPoolerExpose: e.target.value })}>
                      <option value="clusterip">ClusterIP (in-cluster only)</option>
                      <option value="loadbalancer">LoadBalancer (MetalLB address)</option>
                    </select>
                  </Field>
                </>
              )}
            </div>
            <label className="flex items-start gap-2 text-sm">
              <input type="checkbox" className="mt-1" disabled={deployed}
                checked={!!f.k3dCnpgMonitoring}
                onChange={(e) => patchFrame(f.id, { k3dCnpgMonitoring: e.target.checked })} />
              <span>
                Monitor with Prometheus + Grafana
                <span className="block text-xs text-muted">
                  Installs kube-prometheus-stack via Helm into <span className="font-mono">monitoring</span>, then a
                  PodMonitor for this cluster and CloudNativePG's alerting rules. Grafana gets a LoadBalancer
                  address; its admin password comes from <span className="font-mono">GRAFANA_PASSWORD</span>.
                </span>
              </span>
            </label>
            {f.seaweedfsNodeId && (
              <p className="text-xs text-muted">
                Backups go to the selected SeaweedFS bucket with barman-cloud, via the CloudNativePG barman-cloud
                plugin (an ObjectStore resource plus a nightly ScheduledBackup, WAL archiving continuous). That
                plugin needs cert-manager, which is installed alongside it. Unlike the Percona PostgreSQL
                operator, barman-cloud does not require S3 over TLS, so a plain-HTTP SeaweedFS node works.
              </p>
            )}
          </>
        )}
        {/* Expose is per cr.yaml section: the database tier and its front end are independent, so the
            pods can stay in-cluster while the proxy/router takes a LoadBalancer address. */}
        {op === 'pxc' && (
          <>
            <Field label="Proxy" hint="cr.yaml runs one front end — they are mutually exclusive.">
              <select className={`${inputCls} ${lock}`} value={f.k3dProxy || 'haproxy'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dProxy: e.target.value })}>
                <option value="haproxy">HAProxy (default)</option>
                <option value="proxysql">ProxySQL</option>
              </select>
            </Field>
            <Field label="Expose · database (pxc)" hint="Per-pod Services for the database itself.">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposePxc || 'clusterip'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposePxc: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            {(f.k3dProxy || 'haproxy') === 'haproxy' ? (
              <Field label="Expose · HAProxy" hint="The cluster's front door (primary + replicas).">
                <select className={`${inputCls} ${lock}`} value={f.k3dExposeHaproxy || 'loadbalancer'} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dExposeHaproxy: e.target.value })}>
                  {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
                </select>
              </Field>
            ) : (
              <Field label="Expose · ProxySQL" hint="The cluster's front door.">
                <select className={`${inputCls} ${lock}`} value={f.k3dExposeProxysql || 'loadbalancer'} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dExposeProxysql: e.target.value })}>
                  {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
                </select>
              </Field>
            )}
          </>
        )}
        {op === 'ps' && (
          <>
            <Field label="Replication" hint="Async replication is managed by Orchestrator, which adds 3 more pods.">
              <select className={`${inputCls} ${lock}`} value={f.k3dClusterType || 'group-replication'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dClusterType: e.target.value })}>
                <option value="group-replication">Group Replication — 3 MySQL pods (default)</option>
                <option value="async">Async (Orchestrator) — 3 MySQL + 3 Orchestrator pods</option>
              </select>
            </Field>
            <Field label="Proxy" hint="MySQL Router speaks group replication only; HAProxy serves both.">
              <select className={`${inputCls} ${lock}`} value={psFrontEnd} disabled={deployed || psAsync}
                onChange={(e) => patchFrame(f.id, { k3dProxy: e.target.value })}>
                <option value="haproxy">HAProxy (default)</option>
                <option value="router">MySQL Router</option>
              </select>
            </Field>
            <Field label="Expose · database (mysql)" hint="The primary's Service.">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposeMysql || 'clusterip'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposeMysql: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            {psFrontEnd === 'router' ? (
              <Field label="Expose · MySQL Router" hint="The cluster's front door.">
                <select className={`${inputCls} ${lock}`} value={f.k3dExposeRouter || 'loadbalancer'} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dExposeRouter: e.target.value })}>
                  {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
                </select>
              </Field>
            ) : (
              <Field label="Expose · HAProxy" hint="The cluster's front door.">
                <select className={`${inputCls} ${lock}`} value={f.k3dExposeHaproxy || 'loadbalancer'} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dExposeHaproxy: e.target.value })}>
                  {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
                </select>
              </Field>
            )}
          </>
        )}
        {op === 'psmdb' && (
          <>
            <Field label="Topology" hint="Sharding adds 3 config servers + 3 mongos routers on top of the replica set.">
              <select className={`${inputCls} ${lock}`} value={f.k3dSharding ? 'sharded' : 'replicaset'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dSharding: e.target.value === 'sharded' })}>
                <option value="replicaset">Replica set — rs0, 3 pods (default)</option>
                <option value="sharded">Sharded — rs0 + config servers + mongos, 9 pods</option>
              </select>
            </Field>
            <Field label="Expose · replica set" hint="Per-pod Services for the mongod pods.">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposeReplset || 'clusterip'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposeReplset: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            {f.k3dSharding && (
              <Field label="Expose · mongos" hint="The routers — a sharded cluster's front door.">
                <select className={`${inputCls} ${lock}`} value={f.k3dExposeMongos || 'loadbalancer'} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dExposeMongos: e.target.value })}>
                  {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
                </select>
              </Field>
            )}
          </>
        )}
        {op === 'pg' && (
          <>
            <Field label="Expose · PostgreSQL" hint="The primary's Service (the read/write endpoint).">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposePg || 'clusterip'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposePg: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            <Field label="Expose · pgBouncer" hint="The connection pooler — a PGO cluster's front door.">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposePgbouncer || 'loadbalancer'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposePgbouncer: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            <p className="text-xs text-muted">
              pgBackRest speaks S3 only over TLS, so backups need a SeaweedFS node with <span className="font-medium">TLS
              on</span>. Without one the cluster keeps the operator's own PVC backup repo.
            </p>
          </>
        )}
        {pgo && (
          <>
            <div className="grid grid-cols-2 gap-2">
              <Field label="Instances" hint="Postgres pods (1 primary + replicas). Each is a 4-container pod, on top of a pgBackRest repo host and pgBouncer.">
                <input type="number" min="1" max="5" className={`${inputCls} ${lock}`} disabled={deployed}
                  value={f.k3dPgoInstances || 2}
                  onChange={(e) => patchFrame(f.id, { k3dPgoInstances: Number(e.target.value) })} />
              </Field>
              <Field label="Storage (GiB per instance)">
                <input type="number" min="1" max="512" className={`${inputCls} ${lock}`} disabled={deployed}
                  value={f.k3dPgoStorageGb || 1}
                  onChange={(e) => patchFrame(f.id, { k3dPgoStorageGb: Number(e.target.value) })} />
              </Field>
            </div>
            <Field label="PostgreSQL major" hint={f.k3dPgoMonitoring
              ? 'spec.postgresVersion. Crunchy\'s pgMonitor exporter stops at 17 — on 18 the sidecar runs but the operator never creates its monitoring role, so the dashboards stay empty. Blank takes the newest that still works.'
              : 'spec.postgresVersion. Required by the CRD, so blank takes the newest the chart ships an image for.'}>
              {pgMajors.length ? (
                <select className={`${inputCls} ${lock}`} value={f.k3dPgoVersion || ''} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dPgoVersion: e.target.value })}>
                  <option value="">newest{pgMajors[0] ? ` (${pgMajors[0]})` : ''}</option>
                  {pgMajors.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
              ) : (
                <input className={`${inputCls} ${lock}`} value={f.k3dPgoVersion ?? ''} disabled={deployed}
                  placeholder="newest (e.g. 18)"
                  onChange={(e) => patchFrame(f.id, { k3dPgoVersion: e.target.value })} />
              )}
            </Field>
            <Field label="Expose · PostgreSQL" hint="The HA Service in front of the primary (the read/write endpoint).">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposePg || 'clusterip'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposePg: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            <Field label="Expose · pgBouncer" hint="The connection pooler — a PGO cluster's front door.">
              <select className={`${inputCls} ${lock}`} value={f.k3dExposePgbouncer || 'loadbalancer'} disabled={deployed}
                onChange={(e) => patchFrame(f.id, { k3dExposePgbouncer: e.target.value })}>
                {K3D_EXPOSE_OPTIONS.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
            <label className="flex items-start gap-2 text-sm">
              <input type="checkbox" className="mt-1" disabled={deployed}
                checked={!!f.k3dPgoMonitoring}
                onChange={(e) => patchFrame(f.id, { k3dPgoMonitoring: e.target.checked })} />
              <span>
                Monitor with Prometheus + Grafana
                <span className="block text-xs text-muted">
                  Sets <span className="font-mono">spec.monitoring.pgmonitor.exporter</span>, so the operator adds its
                  own crunchy-postgres-exporter sidecar to every instance pod, and installs kube-prometheus-stack
                  into <span className="font-mono">monitoring</span> with a PodMonitor and pgMonitor's PostgreSQL and
                  pgBackRest dashboards. Grafana gets a LoadBalancer address; its admin password comes from{' '}
                  <span className="font-mono">GRAFANA_PASSWORD</span>. Needs PostgreSQL 17 or older — the exporter is
                  not built for 18.
                </span>
              </span>
            </label>
            <p className="text-xs text-muted">
              The cluster gets a <span className="font-mono">postgres</span> superuser and an application user named
              after it, both with <span className="font-mono">POSTGRES_PASSWORD</span> from{' '}
              <span className="font-mono">.env</span>. Connections need{' '}
              <span className="font-mono">sslmode=require</span> — PGO's PostgreSQL and pgBouncer both refuse
              plaintext. Backups use the same pgBackRest as the Percona operator, so S3 needs a SeaweedFS node with
              TLS on; without one the cluster keeps a PVC repo.
            </p>
          </>
        )}
        {op && !pgo && (
          <p className="text-xs text-muted">
            Before <span className="font-mono">cr.yaml</span> is applied, every section's CPU/memory requests are
            commented out{op === 'pg'
              ? ' — the shipped requests do not fit a cluster this size (PostgreSQL\u2019s anti-affinity is already soft, so it needs no change).'
              : ' and anti-affinity is set to none — otherwise the pods never schedule on a cluster this size.'}
          </p>
        )}
      </div>

      {/* Debugging the operator is a deploy-time decision twice over: the debug binary is compiled
          from the operator's own source tarball, and k3d fixes a cluster's published ports when it
          creates it. Both are why this cannot be switched on after the fact. */}
      {op === 'pxc' && (
        <div className="space-y-2 rounded-lg bg-surface2 p-2">
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-1" disabled={deployed}
              checked={!!f.k3dDebug}
              onChange={(e) => patchFrame(f.id, { k3dDebug: e.target.checked })} />
            <span>
              Run the operator under Delve
              <span className="block text-xs text-muted">
                Compiles the operator from the tag's source with the optimiser off
                (<span className="font-mono">-gcflags=all=-N -l</span>), runs that binary under
                <span className="font-mono"> dlv</span>, and publishes the debugger's port to the host so an IDE
                can attach — no <span className="font-mono">kubectl port-forward</span> to keep alive. The pod
                keeps the released image; only its command changes. Adds a few minutes for the build.
              </span>
            </span>
          </label>
          {f.k3dDebug && (
            <>
              <label className="flex items-start gap-2 text-sm">
                <input type="checkbox" className="mt-1" disabled={deployed}
                  checked={!f.k3dDebugNoPublish}
                  onChange={(e) => patchFrame(f.id, { k3dDebugNoPublish: !e.target.checked })} />
                <span>
                  Also publish the debugger to the host, for an external IDE
                  <span className="block text-xs text-muted">
                    Only needed for VS Code or another editor outside DBCanvas. The built-in
                    <span className="font-medium text-fg"> Operator Debugger</span> reaches Delve over the stack
                    network and needs no host port — and the port is fixed, so two clusters debugged at once
                    would collide on it.
                  </span>
                </span>
              </label>
              {!f.k3dDebugNoPublish && (
                <Field label="Debugger port (host)"
                  hint="Fixed rather than auto-assigned — it goes in your IDE's launch.json, and k3d can only publish it while the cluster is being created.">
                  <input type="number" min="1024" max="65535" className={`${inputCls} w-28`} disabled={deployed}
                    value={f.k3dDebugPort || 40000}
                    onChange={(e) => patchFrame(f.id, { k3dDebugPort: Number(e.target.value) })} />
                </Field>
              )}
              <p className="text-xs text-muted">
                Once the cluster is up, open <span className="font-medium text-fg">Operator Debugger</span> in the
                sidebar — breakpoints, call stack and variables, with no IDE to set up.
                {!f.k3dDebugNoPublish && (
                  <> An IDE can attach to <span className="font-mono">127.0.0.1:{f.k3dDebugPort || 40000}</span>{' '}
                  instead; the server node's panel carries the <span className="font-mono">launch.json</span> and the
                  matching <span className="font-mono">git clone</span>.</>
                )}{' '}
                Delve starts with <span className="font-mono">--continue</span>, so the cluster deploys normally
                whether or not anyone ever attaches.
              </p>
            </>
          )}
        </div>
      )}

      <Field label="Backups (SeaweedFS)" hint="Optional — sets the operator's S3 backup storage.">
        <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed}
          onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
          <option value="">none</option>
          {swNodes.map((x) => <option key={x.id} value={x.id}>{x.label}</option>)}
        </select>
      </Field>
      <SeaweedBucketField nodes={nodes} nodeId={f.seaweedfsNodeId} value={f.seaweedfsBucket} deployed={deployed}
        onChange={(v) => patchFrame(f.id, { seaweedfsBucket: v })} />
      {/* PMM monitors a Percona operator's cluster through a pmm-client sidecar that the
          operator's own CR configures. CloudNativePG is not a Percona product and ships no
          such sidecar, so the picker is hidden for it rather than offering monitoring that
          would never arrive — CNPG's monitoring is the Prometheus/Grafana option above.
          A design saved before this was hidden is caught by k3dFrameIssues. */}
      {helmOp ? (
        <p className="text-xs text-muted">
          {cnpg
            ? "CloudNativePG has no PMM integration — it isn't a Percona product and ships no pmm-client sidecar. Use the Prometheus + Grafana option above, which installs kube-prometheus-stack with a PostgreSQL dashboard."
            : "Crunchy PGO has no PMM integration — it isn't a Percona product and ships no pmm-client sidecar. Use the Prometheus + Grafana option above, which turns on the operator's own crunchy-postgres-exporter and loads pgMonitor's dashboards."}
        </p>
      ) : (
        <>
          <Field label="Monitored by (PMM)" hint="Optional — sets spec.pmm.serverHost and wires a service token.">
            <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed}
              onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
              <option value="">none</option>
              {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
            </select>
          </Field>
          {f.pmmNodeId && (
            <>
              <div className="flex items-center gap-2">
                <span className="text-xs text-muted">Service token expires in</span>
                <input type="number" min="1" className={`${inputCls} w-20`} value={f.k3dPmmTokenTtlValue || 365} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dPmmTokenTtlValue: Number(e.target.value) })} />
                <select className={`${inputCls} ${lock}`} value={f.k3dPmmTokenTtlUnit || 'days'} disabled={deployed}
                  onChange={(e) => patchFrame(f.id, { k3dPmmTokenTtlUnit: e.target.value })}>
                  <option value="minutes">minutes</option>
                  <option value="hours">hours</option>
                  <option value="days">days</option>
                </select>
              </div>
              <p className="text-xs text-muted">
                PMM 3 authenticates the pmm-client sidecars with a <span className="font-medium">service token</span>, not a
                password. One is minted on the PMM server at deploy and patched into the cluster's secret; when it expires
                the pods stop reporting until a new one is patched in.
              </p>
            </>
          )}
        </>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// K3DMemberForm — a k3s node. Nothing here is per-node: k3d creates the containers, and the
// cluster's settings live on the frame.
function K3DMemberForm({ node: n, frame, frameNodes, patchNode, dep, deployed }) {
  const isServer = frameNodes.length > 0 && frameNodes[0].id === n.id
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">k3s {isServer ? 'server' : 'agent'}</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>
      <p className="text-xs text-muted">
        {isServer
          ? 'The cluster’s server node. kubectl runs here, and the operator source is unpacked into /root.'
          : 'A worker node. It joins the server automatically.'}
        {' '}Cluster-wide settings (size, CPU/memory, operator) live on the frame.
      </p>
      {!deployed && <p className="text-xs text-muted">Created by k3d at deploy.</p>}
    </div>
  )
}

// ValkeyClusterFrameForm edits a Valkey Cluster frame: catalog OS/version/arch +
// Valkey major/minor, shared default-user password, optional LDAP, PMM monitor.
// 3–7 all-master shards (resize with the frame +/-).
function ValkeyClusterFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = useValkeyCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const count = frameNodes.length

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.valkeyMajor]) || []
  const debian = f.os === 'ubuntu' || f.os === 'debian'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Valkey Cluster</span>
        <Badge tone="muted">{count} node{count === 1 ? '' : 's'}</Badge>
      </div>
      <p className="text-xs text-muted">
        {count} all-master shard{count === 1 ? '' : 's'}, Valkey installed via percona-release, formed with
        <span className="font-mono"> valkey-cli --cluster create</span>. Use the frame +/- to resize (3–7).
      </p>

      <Field label="Cluster name" hint="Frame label; must be unique.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Valkey major">
          <select className={`${inputCls} ${lock}`} value={f.valkeyMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { valkeyMajor: e.target.value, valkeyVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Valkey minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.valkeyVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { valkeyVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useLdap} disabled={deployed || debian} onChange={(e) => patchFrame(f.id, { useLdap: e.target.checked })} />
        <span>Enable LDAP auth (Intranet OpenLDAP)</span>
      </label>
      {debian && <p className="text-xs text-muted">percona-valkey-ldap isn't published for Ubuntu yet — pick Oracle Linux for LDAP auth.</p>}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <Field label="Monitored by (PMM)" hint="Optional — installs/registers pmm-client on each member.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      {count < 3 && <p className="text-xs text-amber-500">A Valkey cluster needs at least 3 nodes.</p>}
      {count > 7 && <p className="text-xs text-amber-500">A Valkey cluster allows at most 7 nodes.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// ValkeyClusterMemberForm edits one Valkey cluster member (label + host-port export).
function ValkeyClusterMemberForm({ node: n, frame: f, patchNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Valkey member</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">Member of <span className="font-mono">{f?.label || 'valkey cluster'}</span>. Auth/LDAP/PMM are set on the cluster frame.</p>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export Valkey port (6379) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
      {!deployed && <p className="text-xs text-muted">Use the frame +/- to add or remove members (3–7).</p>}
    </div>
  )
}

// ProxySQLForm edits a (not-yet-running) ProxySQL node: catalog-driven OS/version
// + ProxySQL major/minor, implementation mode, host-port export and PMM monitor.
// It must be linked to a PXC cluster frame by an association line on the canvas.
function ProxySQLForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.proxysqlCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.proxysqlMajor]) || []

  // Same cascade-normalization as the PXC frame: snap invalid dependent selects.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === n.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(n.proxysqlMajor) ? n.proxysqlMajor : (majorList[0] ?? n.proxysqlMajor)
    if (major !== n.proxysqlMajor) patch.proxysqlMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (n.proxysqlVersion && !minorList.includes(n.proxysqlVersion)) patch.proxysqlVersion = ''
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.proxysqlMajor, n.proxysqlVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  // Walk the association graph (a ProxySQL may reach a cluster through another ProxySQL).
  const linkedFrame = (() => {
    const adj = {}
    for (const e of edges) {
      ;(adj[e.from.node] ||= []).push(e.to.node)
      ;(adj[e.to.node] ||= []).push(e.from.node)
    }
    const seen = new Set([n.id])
    const queue = [n.id]
    while (queue.length) {
      const cur = queue.shift()
      for (const nb of adj[cur] || []) {
        const f = frames.find((fr) => fr.id === nb && (fr.type === 'pxc' || fr.type === 'mysql'))
        if (f) return f
        if (!seen.has(nb)) { seen.add(nb); queue.push(nb) }
      }
    }
    return null
  })()
  const modeOpts = proxyModeOpts(linkedFrame?.type)
  // Normalize the mode when the linked backend changes (PXC vs MySQL modes differ).
  useEffect(() => {
    if (deployed || !linkedFrame) return
    if (!modeOpts.some((m) => m.id === n.mode)) patchNode(n.id, { mode: modeOpts[0].id })
  }, [linkedFrame?.type, n.mode, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">ProxySQL</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      {linkedFrame ? (
        <div className="rounded-lg border border-success/30 bg-success/10 px-2.5 py-1.5 text-xs text-success">
          Linked to {linkedFrame.type === 'mysql' ? 'MySQL' : 'PXC'} cluster <span className="font-semibold">{linkedFrame.label}</span> (data flows cluster → ProxySQL).
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked — drag an association line from a PXC cluster frame to this node.
        </div>
      )}

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="ProxySQL major">
          <select className={`${inputCls} ${lock}`} value={n.proxysqlMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { proxysqlMajor: e.target.value, proxysqlVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>proxysql{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="ProxySQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.proxysqlVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { proxysqlVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Implementation mode" hint={deployed ? 'Locked.' : (modeOpts === PROXY_MODE_OPTS.mysql ? 'How ProxySQL routes traffic to the MySQL primary/replicas.' : 'MODE for proxysql-admin.')}>
        <select className={`${inputCls} ${lock}`} value={modeOpts.some((m) => m.id === n.mode) ? n.mode : modeOpts[0].id} disabled={deployed} onChange={(e) => patchNode(n.id, { mode: e.target.value })}>
          {modeOpts.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers ProxySQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for egress</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Expose ProxySQL ports to the host (6033 MySQL, 6032 admin)</span>
      </label>
      {n.exportEnabled && (
        <Field label="MySQL host port (6033)" hint="0 / empty = random unused port; the admin port (6032) is auto-assigned.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {!deployed && <p className="text-xs text-muted">Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// frameLinkedCluster walks the association graph from a frame/node id to the PXC
// cluster frame it (transitively) reaches, if any.
function frameLinkedCluster(startId, edges, frames) {
  const adj = {}
  for (const e of edges) {
    ;(adj[e.from.node] ||= []).push(e.to.node)
    ;(adj[e.to.node] ||= []).push(e.from.node)
  }
  const seen = new Set([startId])
  const queue = [startId]
  while (queue.length) {
    const cur = queue.shift()
    for (const nb of adj[cur] || []) {
      const f = frames.find((fr) => fr.id === nb && (fr.type === 'pxc' || fr.type === 'mysql'))
      if (f) return f
      if (!seen.has(nb)) { seen.add(nb); queue.push(nb) }
    }
  }
  return null
}

// ProxySQLFrameForm edits a ProxySQL cluster frame: catalog-driven OS/version +
// ProxySQL major/minor, implementation mode, PMM monitor and Intranet-proxy
// options. Per-member host-port export lives on each member node.
function ProxySQLFrameForm({ frame: f, nodes, frames, edges, patchFrame, deleteFrame, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.proxysqlCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.proxysqlMajor]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.proxysqlMajor) ? f.proxysqlMajor : (majorList[0] ?? f.proxysqlMajor)
    if (major !== f.proxysqlMajor) patch.proxysqlMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.proxysqlVersion && !minorList.includes(f.proxysqlVersion)) patch.proxysqlVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.proxysqlMajor, f.proxysqlVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const memberCount = nodes.filter((n) => n.frameId === f.id).length
  const linkedFrame = frameLinkedCluster(f.id, edges, frames)
  const modeOpts = proxyModeOpts(linkedFrame?.type)
  useEffect(() => {
    if (deployed || !linkedFrame) return
    if (!modeOpts.some((m) => m.id === f.mode)) patchFrame(f.id, { mode: modeOpts[0].id })
  }, [linkedFrame?.type, f.mode, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">ProxySQL Cluster</span>
        <Badge tone="primary">{memberCount} node{memberCount === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      {linkedFrame ? (
        <div className="rounded-lg border border-success/30 bg-success/10 px-2.5 py-1.5 text-xs text-success">
          Linked to {linkedFrame.type === 'mysql' ? 'MySQL' : 'PXC'} cluster <span className="font-semibold">{linkedFrame.label}</span> (data flows cluster → ProxySQL).
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked — drag an association line from a PXC cluster frame to this cluster.
        </div>
      )}

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="ProxySQL major">
          <select className={`${inputCls} ${lock}`} value={f.proxysqlMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { proxysqlMajor: e.target.value, proxysqlVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>proxysql{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="ProxySQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.proxysqlVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { proxysqlVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Implementation mode" hint={deployed ? 'Locked.' : (modeOpts === PROXY_MODE_OPTS.mysql ? 'How ProxySQL routes traffic to the MySQL primary/replicas.' : 'MODE for proxysql-admin.')}>
        <select className={`${inputCls} ${lock}`} value={modeOpts.some((m) => m.id === f.mode) ? f.mode : modeOpts[0].id} disabled={deployed} onChange={(e) => patchFrame(f.id, { mode: e.target.value })}>
          {modeOpts.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each member with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <p className="text-xs text-muted">Add/remove ProxySQL nodes with the +/- on the frame. Per-node host-port export is set on each node.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// ProxySQLFrameMemberForm edits a ProxySQL cluster member: only host-port export
// (OS/version/mode come from the frame).
function ProxySQLFrameMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack.">
        <input className={`${inputCls} opacity-70`} value={n.label} readOnly />
      </Field>
      <Field label="ProxySQL cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Expose ProxySQL ports to the host (6033 MySQL, 6032 admin)</span>
      </label>
      {n.exportEnabled && (
        <Field label="MySQL host port (6033)" hint="0 / empty = random unused port; the admin port (6032) is auto-assigned.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
      {!deployed && <p className="text-xs text-muted">Cluster settings (OS, version, mode, monitoring) are on the frame.</p>}
    </div>
  )
}

// InnoDBFrameForm edits an InnoDB Cluster / GR frame: image OS/version/arch,
// the PDPS repository (which sets the Percona Server version), the replication mode
// (InnoDB Cluster vs raw Group Replication), root password, PMM/proxy/cert, and the
// MySQL Router toggle. It has no association endpoints (Router is built in).
function InnoDBFrameForm({ frame: f, nodes, patchFrame, deleteFrame, deployed }) {
  const [imgs, setImgs] = useState([])
  const [repos, setRepos] = useState([])
  useEffect(() => {
    let alive = true
    stackApi.psCatalog().then((c) => { if (alive) setImgs(c.images || []) }).catch(() => { /* */ })
    stackApi.pdpsCatalog().then((c) => { if (alive) setRepos(c.repos || []) }).catch(() => { /* */ })
    return () => { alive = false }
  }, [])
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (deployed || !repos.length) return
    if (!repos.includes(f.pdpsRepo)) patchFrame(f.id, { pdpsRepo: repos[0] })
  }, [repos, f.pdpsRepo, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const members = nodes.filter((x) => x.frameId === f.id).length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">InnoDB Cluster / GR</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <Field label="Replication mode" hint={deployed ? 'Locked.' : 'InnoDB Cluster adds MySQL Shell management + Router metadata.'}>
        <select className={`${inputCls} ${lock}`} value={f.replMode || 'innodbcluster'} disabled={deployed} onChange={(e) => patchFrame(f.id, { replMode: e.target.value })}>
          <option value="innodbcluster">InnoDB Cluster</option>
          <option value="groupreplication">Group Replication</option>
        </select>
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PDPS repository" hint={deployed ? 'Locked.' : 'Sets the Percona Server version.'}>
          <select className={`${inputCls} ${lock}`} value={f.pdpsRepo} disabled={deployed} onChange={(e) => patchFrame(f.id, { pdpsRepo: e.target.value })}>
            {repos.length === 0 && <option value="">(run make versions)</option>}
            {repos.map((r) => <option key={r} value={r}>{r}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Monitored by (PMM)" hint="Optional — registers each node with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.mysqlRouter !== false} disabled={deployed} onChange={(e) => patchFrame(f.id, { mysqlRouter: e.target.checked })} />
        <span>Install MySQL Router on each node (6446 RW / 6447 RO)</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {members < 3 && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          At least 3 nodes are recommended for Group Replication quorum ({members} now).
        </div>
      )}
      <p className="text-xs text-muted">No association line — MySQL Router is built in. Per-node host-port export is set on each node.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// InnoDBMemberForm edits an InnoDB/GR member: only host-port export of the router
// ports (OS/version/mode come from the frame; GR auto-elects the primary).
function InnoDBMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">Group Replication auto-elects the primary; secondaries are read-only.</p>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export MySQL Router ports to the host (6446 RW / 6447 RO)</span>
      </label>
      {n.exportEnabled && (
        <Field label="RW host port (6446)" hint="0 / empty = random unused port; the RO port (6447) is auto-assigned.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// PBMOptions renders the "Enable Percona Backup for MongoDB" checkbox + the
// SeaweedFS-node selector, shared by the PSMDB sharded-cluster and replica-set
// frame forms. percona-backup-mongodb is installed on every member regardless;
// enabling this configures pbm-agent + the S3 store on the selected SeaweedFS node.
function PBMOptions({ f, nodes, patchFrame, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const seaweedNodes = nodes.filter((n) => n.type === 'seaweedfs')
  return (
    <>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.enablePBM} disabled={deployed} onChange={(e) => patchFrame(f.id, { enablePBM: e.target.checked })} />
        <span>Enable backups with Percona Backup for MongoDB (PBM)</span>
      </label>
      {f.enablePBM && (
        <Field label="SeaweedFS node (S3 backup storage)" hint={seaweedNodes.length ? 'pbm-agent runs on every member; backups land in this node\'s S3 bucket.' : 'Add a SeaweedFS node to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
          </select>
        </Field>
      )}
      {!!f.seaweedfsNodeId && (
        <SeaweedBucketField nodes={nodes} nodeId={f.seaweedfsNodeId} value={f.seaweedfsBucket} deployed={deployed}
          onChange={(v) => patchFrame(f.id, { seaweedfsBucket: v })} />
      )}
    </>
  )
}

// MongoDBFrameForm edits a PSMDB Sharded Cluster frame: catalog-driven
// OS/version/arch + PS MongoDB major/minor, admin (root) password, PMM/proxy/cert.
// The 13-node sharded topology is fixed — there are no replication options.
function MongoDBFrameForm({ frame: f, nodes, patchFrame, deleteFrame, rebuildCluster, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psmdbCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.psmdbMajor]) || []

  // Cascade-normalize the selection when a higher-level field changes (same logic
  // as PXCFrameForm), so major/minor never go stale for the chosen image.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.psmdbMajor) ? f.psmdbMajor : (majorList[0] ?? f.psmdbMajor)
    if (major !== f.psmdbMajor) patch.psmdbMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.psmdbVersion && !minorList.includes(f.psmdbVersion)) patch.psmdbVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.psmdbMajor, f.psmdbVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const total = nodes.filter((n) => n.frameId === f.id).length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PSMDB Sharded Cluster</span>
        <Badge tone="primary">{total} node{total === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <Field label="Setup" hint={deployed ? 'Locked.' : 'Standard is HA; minimum is the smallest working sharded cluster.'}>
        <select className={`${inputCls} ${lock}`} value={f.psmdbSetup || 'standard'} disabled={deployed}
          onChange={(e) => rebuildCluster?.(f.id, e.target.value)}>
          <option value="standard">standard — 3 shards × 3-node RS + 3-node config RS (13 nodes)</option>
          <option value="minimum">minimum — 3 single-node shards + 1 config server (5 nodes)</option>
        </select>
      </Field>

      <div className="rounded-lg border border-dashed px-2.5 py-1.5 text-xs text-muted">
        {(f.psmdbSetup || 'standard') === 'minimum'
          ? '3 single-node shards + 1 config server + 1 mongos router. Nodes can\'t be added or removed.'
          : '3 shards × 3-node replica set (9 mongod) + 3-node config-server replica set + 1 mongos router. Nodes can\'t be added or removed.'}
      </div>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PS MongoDB major">
          <select className={`${inputCls} ${lock}`} value={f.psmdbMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { psmdbMajor: e.target.value, psmdbVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PS MongoDB minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.psmdbVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { psmdbVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each node with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <PBMOptions f={f} nodes={nodes} patchFrame={patchFrame} deployed={deployed} />

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <p className="text-xs text-muted">Apps connect through the mongos router; enable host-port export on the mongos node to reach it from the host.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// MongoDBMemberForm edits a PS MongoDB member: read-only role/shard (the topology
// is fixed); only the mongos router can export its 27017 port to the host.
function MongoDBMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  const roleText = n.role === 'mongos' ? 'mongos router' : n.role === 'config' ? 'config server' : `shard ${n.shard} member`
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <Field label="Role"><input className={`${inputCls} opacity-70`} value={roleText} readOnly /></Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      {n.role === 'mongos' ? (
        <>
          <p className="text-xs text-muted">The mongos router is the cluster entry point; export 27017 so apps connect from the host.</p>
          <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
            <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
            <span>Export mongos port to the host (27017)</span>
          </label>
          {n.exportEnabled && (
            <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
              <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
                onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
            </Field>
          )}
        </>
      ) : (
        <p className="text-xs text-muted">Shard and config-server members are internal to the cluster — connect through the mongos router.</p>
      )}
    </div>
  )
}

// MongoCatalogFields renders the shared catalog-driven OS/version/arch + PS MongoDB
// major/minor selects used by both the PSMDB RS frame form (patch=patchFrame, obj=frame)
// and the standalone PSMDB node form (patch=patchNode, obj=node). `patch(id, {...})`
// applies the change. Cascade-normalizes invalid dependent selects like PXCFrameForm.
function useMongoCatalog(obj, deployed, patch) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psmdbCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
  useEffect(() => {
    if (deployed || !imgs.length) return
    const p = {}
    const osVer = osVersions.includes(obj.osVersion) ? obj.osVersion : (osVersions[0] ?? obj.osVersion)
    if (osVer !== obj.osVersion) p.osVersion = osVer
    const e2 = imgs.find((i) => i.os === obj.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(obj.psmdbMajor) ? obj.psmdbMajor : (majorList[0] ?? obj.psmdbMajor)
    if (major !== obj.psmdbMajor) p.psmdbMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (obj.psmdbVersion && !minorList.includes(obj.psmdbVersion)) p.psmdbVersion = ''
    if (Object.keys(p).length) patch(obj.id, p)
  }, [imgs, obj.id, obj.os, obj.osVersion, obj.psmdbMajor, obj.psmdbVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps
  return imgs
}

function MongoCatalogFields({ obj, imgs, deployed, patch }) {
  const lock = deployed ? 'opacity-70' : ''
  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === obj.os && i.osVersion === obj.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[obj.psmdbMajor]) || []
  return (
    <>
      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={obj.os} disabled={deployed} onChange={(e) => patch(obj.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={obj.osVersion} disabled={deployed} onChange={(e) => patch(obj.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PS MongoDB major">
          <select className={`${inputCls} ${lock}`} value={obj.psmdbMajor} disabled={deployed} onChange={(e) => patch(obj.id, { psmdbMajor: e.target.value, psmdbVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PS MongoDB minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={obj.psmdbVersion} disabled={deployed} onChange={(e) => patch(obj.id, { psmdbVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>
    </>
  )
}

// PSMRSFrameForm edits a PS MongoDB replica-set frame: catalog OS/version/arch + PS
// MongoDB major/minor, admin password, PMM/proxy/cert. Members are resizable 1–9.
function PSMRSFrameForm({ frame: f, nodes, patchFrame, deleteFrame, deployed }) {
  const imgs = useMongoCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const members = nodes.filter((n) => n.frameId === f.id).length
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PSMDB RS</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Replica-set name" hint="Becomes the replica-set name; must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <MongoCatalogFields obj={f} imgs={imgs} deployed={deployed} patch={patchFrame} />

      <Field label="Monitored by (PMM)" hint="Optional — registers each member with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <PBMOptions f={f} nodes={nodes} patchFrame={patchFrame} deployed={deployed} />

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 3 || members % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 3 && <div>At least 3 members are recommended for election quorum ({members} now).</div>}
          {members % 2 === 0 && <div>An odd number of members keeps quorum on a split network ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Use the +/− buttons on the frame to resize the replica set (1–9 members).</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete replica set
      </Button>
    </div>
  )
}

// PSMRSMemberForm edits a PS MongoDB replica-set member: only host-port export
// (OS/version come from the frame; the replica set auto-elects the primary).
function PSMRSMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Replica set"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <p className="text-xs text-muted">The replica set auto-elects the primary; secondaries serve reads.</p>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export mongod port to the host (27017)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// usePPGCatalog loads the Percona PostgreSQL catalog and cascade-normalizes a
// frame's OS/version/arch + PG major/minor selects (same shape as useMongoCatalog).
function usePPGCatalog(obj, deployed, patch, fetchCat = stackApi.ppgCatalog) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    fetchCat().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
  useEffect(() => {
    if (deployed || !imgs.length) return
    const p = {}
    const osVer = osVersions.includes(obj.osVersion) ? obj.osVersion : (osVersions[0] ?? obj.osVersion)
    if (osVer !== obj.osVersion) p.osVersion = osVer
    const e2 = imgs.find((i) => i.os === obj.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(obj.pgMajor) ? obj.pgMajor : (majorList[0] ?? obj.pgMajor)
    if (major !== obj.pgMajor) p.pgMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (obj.pgVersion && !minorList.includes(obj.pgVersion)) p.pgVersion = ''
    if (Object.keys(p).length) patch(obj.id, p)
  }, [imgs, obj.id, obj.os, obj.osVersion, obj.pgMajor, obj.pgVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps
  return imgs
}

// useValkeyCatalog is usePPGCatalog's cascade-normalization shape, but keyed on
// valkeyMajor/valkeyVersion (Valkey's own field names) instead of pgMajor/pgVersion,
// backed by the Valkey package catalog (/api/catalog/valkey). Shared by ValkeyForm
// (a node) and ValkeyClusterFrameForm (a frame).
function useValkeyCatalog(obj, deployed, patch) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.valkeyCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
  useEffect(() => {
    if (deployed || !imgs.length) return
    const p = {}
    const osVer = osVersions.includes(obj.osVersion) ? obj.osVersion : (osVersions[0] ?? obj.osVersion)
    if (osVer !== obj.osVersion) p.osVersion = osVer
    const e2 = imgs.find((i) => i.os === obj.os && i.osVersion === osVer)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(obj.valkeyMajor) ? obj.valkeyMajor : (majorList[0] ?? obj.valkeyMajor)
    if (major !== obj.valkeyMajor) p.valkeyMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (obj.valkeyVersion && !minorList.includes(obj.valkeyVersion)) p.valkeyVersion = ''
    if (Object.keys(p).length) patch(obj.id, p)
  }, [imgs, obj.id, obj.os, obj.osVersion, obj.valkeyMajor, obj.valkeyVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps
  return imgs
}

// useSpockCatalog is usePPGCatalog backed by the Spock (source-built) catalog
// (/api/catalog/spock) instead of the Percona PostgreSQL *package* catalog. Spock
// compiles PostgreSQL from source, so its available majors/minors and OS/platforms
// differ from PPG packages (e.g. it is offered on Oracle Linux 8, which has no PPG
// packages). Same cascade-normalization shape.
function useSpockCatalog(obj, deployed, patch) {
  return usePPGCatalog(obj, deployed, patch, stackApi.spockCatalog)
}

// PatroniFrameForm edits a Patroni PostgreSQL cluster frame: catalog OS/version/arch
// + PG major/minor, superuser password, optional pgBackRest → SeaweedFS S3 backup,
// PMM/proxy/cert. Members are resizable 3–7 (etcd quorum; odd recommended).
function PatroniFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = usePPGCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const seaweedNodes = nodes.filter((n) => n.type === 'seaweedfs')
  const members = frameNodes.length

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Patroni Cluster</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Becomes the Patroni scope + pgBackRest stanza; must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major">
          <select className={`${inputCls} ${lock}`} value={f.pgMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pgVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.usePgBackRest} disabled={deployed} onChange={(e) => patchFrame(f.id, { usePgBackRest: e.target.checked })} />
        <span>Use pgBackRest (SeaweedFS S3) for cloning + backup</span>
      </label>
      {f.usePgBackRest && (
        <Field label="SeaweedFS node (S3 repository)" hint={seaweedNodes.length ? 'WAL archive + initial full backup land here; replicas clone via pgBackRest. The node must have S3 TLS enabled (pgBackRest needs HTTPS).' : 'Add a SeaweedFS node (with S3 TLS enabled) to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}{s.tls ? '' : ' — needs S3 TLS'}</option>)}
          </select>
        </Field>
      )}
      {!!f.seaweedfsNodeId && (
        <SeaweedBucketField nodes={nodes} nodeId={f.seaweedfsNodeId} value={f.seaweedfsBucket} deployed={deployed}
          onChange={(v) => patchFrame(f.id, { seaweedfsBucket: v })} />
      )}

      <Field label="Monitored by (PMM)" hint="Optional — registers each member's PostgreSQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 3 || members > 7 || members % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 3 && <div>At least 3 members are required for etcd quorum ({members} now).</div>}
          {members > 7 && <div>At most 7 members are allowed ({members} now).</div>}
          {members % 2 === 0 && members >= 3 && members <= 7 && <div>An odd number of members keeps etcd quorum on a split network ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Each member runs PostgreSQL + Patroni + an etcd member. Use the +/− buttons on the frame to resize (3–7 members). Link an HAProxy node to route writes → leader and reads → replicas.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// PatroniMemberForm edits a Patroni cluster member: only host-port export of 5432
// (OS/version come from the frame; Patroni auto-elects the leader).
function PatroniMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">Runs PostgreSQL + Patroni + an etcd member. Patroni auto-elects the leader; replicas stream from it.</p>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export PostgreSQL port to the host (5432)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// RepmgrFrameForm edits a repmgr PostgreSQL cluster frame: catalog OS/version/arch +
// PG major/minor, superuser password, optional Barman cloud → SeaweedFS S3 backup,
// PMM/proxy/cert. Members are resizable 3–7.
function RepmgrFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = usePPGCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const seaweedNodes = nodes.filter((n) => n.type === 'seaweedfs')
  const members = frameNodes.length

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">repmgr Cluster</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Becomes the Barman server name; must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major">
          <select className={`${inputCls} ${lock}`} value={f.pgMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pgVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useBarman} disabled={deployed} onChange={(e) => patchFrame(f.id, { useBarman: e.target.checked })} />
        <span>Use Barman (SeaweedFS S3) for backups</span>
      </label>
      {f.useBarman && (
        <Field label="SeaweedFS node (S3 backup storage)" hint={seaweedNodes.length ? 'WAL archive + base backups land here via barman-cloud (works over HTTP or HTTPS).' : 'Add a SeaweedFS node to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
          </select>
        </Field>
      )}
      {!!f.seaweedfsNodeId && (
        <SeaweedBucketField nodes={nodes} nodeId={f.seaweedfsNodeId} value={f.seaweedfsBucket} deployed={deployed}
          onChange={(v) => patchFrame(f.id, { seaweedfsBucket: v })} />
      )}

      <Field label="Monitored by (PMM)" hint="Optional — registers each member's PostgreSQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 3 || members > 7 || members % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 3 && <div>At least 3 members are required ({members} now).</div>}
          {members > 7 && <div>At most 7 members are allowed ({members} now).</div>}
          {members % 2 === 0 && members >= 3 && members <= 7 && <div>An odd number of members keeps a clear quorum on a split network ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Each member runs PostgreSQL + repmgr; member 1 starts as the primary and the rest stream as standbys. repmgrd handles automatic failover. Use the +/− buttons on the frame to resize (3–7 members).</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// RepmgrMemberForm edits a repmgr cluster member: only host-port export of 5432
// (OS/version come from the frame; repmgr manages roles + failover).
function RepmgrMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">Runs PostgreSQL + repmgr. The cluster's first node bootstraps as primary; this node streams from it (repmgr can fail over to it).</p>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export PostgreSQL port to the host (5432)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// SpockFrameForm edits a Spock PostgreSQL cluster frame: catalog OS/version/arch + PG
// major/minor, PMM/proxy/cert. Every member is writable (full-mesh active-active). 2–7
// members; no odd-count requirement (no quorum/failover). Spock is compiled from source.
function SpockFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = useSpockCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const members = frameNodes.length

  // Spock compiles PostgreSQL from source with its patches — Oracle Linux only for now.
  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))].filter((o) => o === 'oraclelinux')
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion)
  // Spock 5.x supports PG 15/16/17 — restrict the major picker accordingly.
  const majors = (entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []).filter((m) => Number(m) >= 15)
  const minors = (entry?.versions?.[f.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Spock Cluster</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major" hint="Spock supports 15–17.">
          <select className={`${inputCls} ${lock}`} value={f.pgMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pgVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each member's PostgreSQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 2 || members > 7) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 2 && <div>At least 2 members are required ({members} now).</div>}
          {members > 7 && <div>At most 7 members are allowed ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Every member compiles a <span className="text-fg/80">patched PostgreSQL from source</span> (Spock's patches) plus the pgEdge Spock extension — a full-mesh active-active cluster where any node is writable and changes replicate to all others (last-update-wins conflicts). A demo database <span className="font-mono">spockdemo</span> is set up for replication. Oracle Linux only; the source build adds several minutes per node. Use the +/− buttons to resize (2–7 members).</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// SpockMemberForm edits a Spock cluster member: only host-port export of 5432 (OS/version
// come from the frame; every member is an equal writable node in the mesh).
function SpockMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">PostgreSQL + Spock — a writable member of the active-active mesh. Writes here replicate to every peer, and it receives their writes too.</p>
      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export PostgreSQL port to the host (5432)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// HAProxyForm edits a (not-yet-running) HAProxy node: it must be linked to exactly one
// Patroni or PXC cluster frame by an association line (mutually exclusive). Image
// OS/version/arch come from the generic images catalog; host-port export publishes the
// write/read/stats ports.
function HAProxyForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.imagesCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  // HAProxy is a product install, so the Debian bases the matrix builds for the Linux
  // Client are filtered out (see PRODUCT_OS_FAMILIES).
  const osFamilies = [...new Set(imgs.map((i) => i.os))].filter((o) => PRODUCT_OS_FAMILIES.includes(o))
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]

  // Snap invalid dependent selects once the catalog loads.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  // Directly-linked backend cluster frame(s). HAProxy fronts exactly one — a Patroni
  // PostgreSQL cluster OR a PXC cluster (mutually exclusive).
  const linkedFrames = (() => {
    const out = []
    const seen = new Set()
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const f = frames.find((fr) => fr.id === other && (fr.type === 'patroni' || fr.type === 'pxc'))
      if (f && !seen.has(f.id)) { seen.add(f.id); out.push(f) }
    }
    return out
  })()
  const linkedFrame = linkedFrames.length === 1 ? linkedFrames[0] : null
  const isPXC = linkedFrame?.type === 'pxc'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">HAProxy</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      {linkedFrames.length > 1 ? (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          Linked to multiple clusters — HAProxy fronts exactly one (Patroni and PXC are mutually exclusive). Remove the extra association line.
        </div>
      ) : linkedFrame ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Routes to {isPXC ? 'PXC' : 'Patroni'} cluster <span className="font-mono font-medium">{linkedFrame.label}</span> — {isPXC ? 'writes → single writer (:5000), reads → round-robin (:5001)' : 'writes → leader (:5000), reads → replicas (:5001)'}.
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a Patroni or PXC cluster frame to this HAProxy node.
        </div>
      )}

      <Field label="Label"><input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} /></Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Monitored by (PMM)" hint="Optional — registers the HAProxy service with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export ports to the host (write 5000 / read 5001 / stats 7000)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Write (leader) host port" hint="0 / empty = random unused port. The read + stats ports get random host ports.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// OrchestratorForm edits a (not-yet-running) Percona Orchestrator node: catalog
// OS/version/arch, an optional alert-email mailbox, and the export toggle for its
// web UI (:3000). Unlike HAProxy it carries no canvas association — a PXC or MySQL
// replication frame optionally points at it via its own "Monitored by
// (Orchestrator)" picker (see PXCFrameForm / MySQLFrameForm), so there is no
// linked-cluster banner here.
function OrchestratorForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.orchestratorCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  // No "major" split (Orchestrator isn't versioned per MySQL series) — a single
  // catalog key (currently "3") carries the installable minors.
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion)
  const versionKey = entry ? Object.keys(entry.versions || {})[0] : null
  const versions = (versionKey && entry.versions[versionKey]) || []

  // Snap invalid dependent selects once the catalog loads.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    const e2 = imgs.find((i) => i.os === n.os && i.osVersion === osVer)
    const vk = e2 ? Object.keys(e2.versions || {})[0] : null
    const vList = (vk && e2.versions[vk]) || []
    if (n.orchestratorVersion && !vList.includes(n.orchestratorVersion)) patch.orchestratorVersion = ''
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.orchestratorVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Orchestrator</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />

      <Field label="Label"><input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} /></Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Orchestrator version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.orchestratorVersion || ''} disabled={deployed}
          onChange={(e) => patchNode(n.id, { orchestratorVersion: e.target.value })}>
          <option value="">latest{versions[0] ? ` (${versions[0]})` : ''}</option>
          {versions.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Alert email" hint="A mailbox on the stack's Intranet domain (or a full address) that failure-detection alerts are emailed to. Defaults to admin, which the Intranet always provisions; clear it to disable alerts.">
        <input className={`${inputCls} ${lock}`} placeholder="admin" value={n.alertEmail || ''} disabled={deployed}
          onChange={(e) => patchNode(n.id, { alertEmail: e.target.value })} />
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <p className="text-xs text-muted">The web UI (:3000) is always published to the host, like PMM.</p>

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// OrchestratorManager is the running-node detail view: a link to Orchestrator's web
// UI (published host port), same "location.hostname + cfg port" idiom as VNCManager.
function OrchestratorManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const url = cfg.exportPort ? `http://${host}:${cfg.exportPort}/` : null
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Orchestrator</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">MySQL replication topology visualization and failure detection.</p>
      {url ? (
        <a href={url} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open Orchestrator
        </a>
      ) : (
        <div className="rounded-lg border border-dashed px-2.5 py-1.5 text-xs text-muted">
          Host port not recorded yet — refresh once the node finishes provisioning.
        </div>
      )}
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Version</span><span className="font-mono text-xs">{cfg.version || 'latest'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Alert email</span><span className="font-mono text-xs">{cfg.alertEmail || 'none'}</span></div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// PSMStandaloneForm edits a standalone PS MongoDB node: catalog OS/version/arch + PS
// MongoDB major/minor, admin password, PMM/proxy/cert and host export. (Same options
// as the replica-set frame, minus replication.)
function PSMStandaloneForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const imgs = useMongoCatalog(n, deployed, patchNode)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const keycloakNodes = nodes.filter((x) => x.type === 'keycloak')
  const dirAuthOn = !!n.ldapAuth || !!n.kerberosAuth
  const dirAuthLabel = n.ldapAuth && n.kerberosAuth ? 'LDAP and Kerberos' : n.ldapAuth ? 'LDAP' : 'Kerberos'
  const oidcBlocks = n.enableOIDC ? 'MongoDB cannot do directory authentication and Keycloak OIDC at once — turn off Keycloak SSO above to use LDAP or Kerberos.' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PSMDB (standalone)</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <VMSizeFields node={n} patchNode={patchNode} deployed={deployed} />
      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <MongoCatalogFields obj={n} imgs={imgs} deployed={deployed} patch={patchNode} />

      <Field label="Monitored by (PMM)" hint="Optional — registers this server with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Generate certificate from Intranet CA</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export mongod port (27017) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {/* Keycloak OIDC excludes LDAP/Kerberos on MongoDB: OIDC and directory auth each render
          their own mongod.conf setParameter block, and mongod.conf can only carry one. LDAP and
          Kerberos share a block, so those two coexist. */}
      <div className="rounded-md border border-border/60 p-2 space-y-2">
        <label className={`flex items-center gap-2 text-sm ${deployed || dirAuthOn ? 'opacity-70' : ''}`}>
          <input type="checkbox" checked={!!n.enableOIDC} disabled={deployed || dirAuthOn} onChange={(e) => patchNode(n.id, { enableOIDC: e.target.checked })} />
          <span>Keycloak OIDC authentication (MONGODB-OIDC)</span>
        </label>
        {dirAuthOn && <p className="text-xs text-muted">MongoDB cannot do directory authentication and Keycloak OIDC at once — turn off {dirAuthLabel} below to use Keycloak SSO.</p>}
        {n.enableOIDC && (
          <div className="space-y-2 pl-1">
            <Field label="Keycloak node" hint={keycloakNodes.length ? 'OIDC identity provider for this MongoDB.' : 'Add a Keycloak node first.'}>
              <select className={`${inputCls} ${lock}`} value={n.keycloakNodeId || ''} disabled={deployed || keycloakNodes.length === 0} onChange={(e) => patchNode(n.id, { keycloakNodeId: e.target.value })}>
                <option value="">none</option>
                {keycloakNodes.map((k) => <option key={k.id} value={k.id}>{k.label}</option>)}
              </select>
            </Field>
            <Field label="Realm" hint="Keycloak realm holding the OIDC client.">
              <input className={`${inputCls} ${lock}`} value={n.oidcRealm ?? 'mongodb'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcRealm: e.target.value })} />
            </Field>
            <Field label="Client ID" hint="OIDC client id; also used as the token audience.">
              <input className={`${inputCls} ${lock}`} value={n.oidcClientId ?? 'mongodb-client'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcClientId: e.target.value })} />
            </Field>
            <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
              <input type="checkbox" checked={n.oidcUseAuthClaim !== false} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcUseAuthClaim: e.target.checked })} />
              <span>Authorize by group claim</span>
            </label>
            {n.oidcUseAuthClaim !== false ? (
              <Field label="Authorization claim" hint="Token claim with the user's groups. Creates keycloak/developers + keycloak/dbadmins roles.">
                <input className={`${inputCls} ${lock}`} value={n.oidcAuthClaim ?? 'MyClaim'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcAuthClaim: e.target.value })} />
              </Field>
            ) : (
              <p className="text-xs text-muted">Users are authorized by username — create them in the <span className="font-mono">$external</span> database after deploy.</p>
            )}
          </div>
        )}
      </div>

      <DirectoryAuthFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} kerberos={true}
        ldapBlocked={oidcBlocks} kerberosBlocked={oidcBlocks} />

      <VaultFields node={n} nodes={nodes} patchNode={patchNode} deployed={deployed} />

      {!deployed && <p className="text-xs text-muted">Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// Minimap: a scaled overview of the canvas in the bottom-right corner showing
// every node (colored by type) and the current viewport. Click or drag inside it
// to recenter the main view on that point.
const MINI_W = 184
const MINI_H = 124
const MINI_PAD = 8

function Minimap({ nodes, view, setView, wrapRef, selectedId }) {
  const drag = useRef(false)
  const rect = wrapRef.current?.getBoundingClientRect()
  const vw = rect?.width || 800
  const vh = rect?.height || 600

  // Current viewport expressed in world coordinates.
  const viewWorld = { x: -view.x / view.z, y: -view.y / view.z, w: vw / view.z, h: vh / view.z }

  // Bounds over all nodes plus the viewport, so both are always visible.
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
  for (const n of nodes) {
    minX = Math.min(minX, n.x); minY = Math.min(minY, n.y)
    maxX = Math.max(maxX, n.x + NODE_W); maxY = Math.max(maxY, n.y + NODE_H)
  }
  minX = Math.min(minX, viewWorld.x); minY = Math.min(minY, viewWorld.y)
  maxX = Math.max(maxX, viewWorld.x + viewWorld.w); maxY = Math.max(maxY, viewWorld.y + viewWorld.h)
  if (!isFinite(minX)) { minX = 0; minY = 0; maxX = 1; maxY = 1 }

  const bw = (maxX - minX) || 1
  const bh = (maxY - minY) || 1
  const scale = Math.min((MINI_W - 2 * MINI_PAD) / bw, (MINI_H - 2 * MINI_PAD) / bh)
  const ox = MINI_PAD + ((MINI_W - 2 * MINI_PAD) - bw * scale) / 2 - minX * scale
  const oy = MINI_PAD + ((MINI_H - 2 * MINI_PAD) - bh * scale) / 2 - minY * scale
  const tx = (x) => ox + x * scale
  const ty = (y) => oy + y * scale

  function recenter(e) {
    const r = e.currentTarget.getBoundingClientRect()
    const wx = (e.clientX - r.left - ox) / scale
    const wy = (e.clientY - r.top - oy) / scale
    setView((v) => ({ ...v, x: vw / 2 - wx * v.z, y: vh / 2 - wy * v.z }))
  }

  return (
    <div
      className="absolute bottom-3 right-3 overflow-hidden rounded-lg border bg-surface/90 shadow backdrop-blur"
      style={{ width: MINI_W, height: MINI_H }}
      title="Minimap — click or drag to navigate"
    >
      <svg
        width={MINI_W}
        height={MINI_H}
        className="cursor-pointer"
        style={{ touchAction: 'none' }}
        onPointerDown={(e) => { e.stopPropagation(); drag.current = true; recenter(e) }}
        onPointerMove={(e) => { if (drag.current) recenter(e) }}
        onPointerUp={() => { drag.current = false }}
        onPointerLeave={() => { drag.current = false }}
      >
        <rect
          x={tx(viewWorld.x)} y={ty(viewWorld.y)}
          width={viewWorld.w * scale} height={viewWorld.h * scale}
          fill="var(--primary)" fillOpacity="0.12" stroke="var(--primary)" strokeWidth="1"
        />
        {nodes.map((n) => {
          const def = NODE_TYPES[n.type] || {}
          const on = selectedId === n.id
          return (
            <rect
              key={n.id}
              x={tx(n.x)} y={ty(n.y)}
              width={Math.max(2, NODE_W * scale)} height={Math.max(2, NODE_H * scale)}
              rx="1"
              fill={def.color || 'var(--muted)'} fillOpacity={on ? 1 : 0.8}
              stroke={on ? 'var(--fg)' : 'none'} strokeWidth="1"
            />
          )
        })}
      </svg>
    </div>
  )
}

function PortHandles({ ownerId, connecting, snapPort, onStart }) {
  const pos = {
    top: '-top-2 left-1/2 -translate-x-1/2',
    right: '-right-2 top-1/2 -translate-y-1/2',
    bottom: '-bottom-2 left-1/2 -translate-x-1/2',
    left: '-left-2 top-1/2 -translate-y-1/2',
  }
  return (
    <>
      {PORTS.map((port) => {
        const snap = snapPort === port
        return (
          <button
            key={port}
            onPointerDown={(e) => onStart(e, ownerId, port)}
            className={`absolute h-3 w-3 rounded-full border-2 border-primary bg-surface transition ${pos[port]} ${connecting ? 'opacity-100' : 'opacity-0 group-hover:opacity-100'} ${snap ? 'pulse-ring scale-150 bg-primary' : ''}`}
          />
        )
      })}
    </>
  )
}

// fmtBytes mirrors humanLimit() in app/syssettings.go: exact binary multiples
// read as whole numbers ("4 GiB"), so the limit the settings page shows and the
// limit a refusal quotes are written the same way.
function fmtBytes(n) {
  const U = [[1024 ** 4, 'TiB'], [1024 ** 3, 'GiB'], [1024 ** 2, 'MiB'], [1024, 'KiB']]
  for (const [size, label] of U) {
    if (n >= size) {
      const v = n / size
      return `${Number.isInteger(v) ? v : v.toFixed(1)} ${label}`
    }
  }
  return `${n} bytes`
}

// copyText writes to the clipboard, returning whether it landed. The async
// Clipboard API needs a secure context, and DBCanvas is routinely reached over
// plain http on a lab host's LAN address — so fall back to the old
// select-a-textarea trick there rather than failing.
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch { /* fall through */ }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}

// collectDroppedFiles flattens a drop into [{ path, file }], where path is
// relative to the destination the user will pick. Dropping a folder is worth
// supporting — people drag a whole config or dump directory — so the entries
// API is walked recursively when the browser offers it, with dataTransfer.files
// as the flat fallback. The entries must be taken *synchronously* off the
// DataTransfer (they are invalidated once the drop event finishes), which is
// why the sync sweep happens before the first await.
async function collectDroppedFiles(dt) {
  const entries = []
  for (const item of Array.from(dt.items || [])) {
    if (item.kind !== 'file') continue
    const entry = item.webkitGetAsEntry?.()
    if (entry) entries.push(entry)
  }
  if (entries.length === 0) {
    return Array.from(dt.files || []).map((file) => ({ path: file.name, file }))
  }
  const out = []
  const walk = async (entry, prefix) => {
    const p = prefix ? `${prefix}/${entry.name}` : entry.name
    if (entry.isFile) {
      const file = await new Promise((res, rej) => entry.file(res, rej))
      out.push({ path: p, file })
      return
    }
    if (!entry.isDirectory) return
    // readEntries returns at most ~100 per call and signals the end with an
    // empty batch, so it has to be drained in a loop.
    const reader = entry.createReader()
    for (;;) {
      const batch = await new Promise((res, rej) => reader.readEntries(res, rej))
      if (batch.length === 0) break
      for (const child of batch) await walk(child, p)
    }
  }
  for (const entry of entries) await walk(entry, '')
  // Empty directories carry nothing to copy and tar-ing them alone would be a
  // surprise; the server only ever sees files.
  return out
}

// NodeDropMenu is the destination picker that opens where the files landed, and
// then reports the result in place.
function NodeDropMenu({ drop, node, onPick, onClose }) {
  const x = Math.max(8, Math.min(drop.x, window.innerWidth - 248))
  const y = Math.max(8, Math.min(drop.y, window.innerHeight - 228))
  const label = node?.label || 'node'
  const n = drop.files?.length || 0
  return createPortal(
    <div className="fixed inset-0 z-50" onClick={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }}>
      <div className="absolute w-60 rounded-lg border bg-surface p-1 shadow-xl" style={{ left: x, top: y }} onClick={(e) => e.stopPropagation()}>
        {drop.phase === 'choose' && (
          <>
            <div className="px-2.5 pb-1 pt-1.5 text-[11px] leading-snug text-muted">
              Copy {n} file{n === 1 ? '' : 's'}{drop.total ? ` (${fmtBytes(drop.total)})` : ''} to{' '}
              <span className="font-medium text-fg">{label}</span> at:
            </div>
            {NODE_UPLOAD_DESTS.map((d) => (
              <button key={d} onClick={() => onPick(d)}
                className="block w-full rounded-md px-2.5 py-1.5 text-left font-mono text-sm text-fg hover:bg-surface2">
                {d}
              </button>
            ))}
            <div className="my-1 h-px bg-border" />
            <button onClick={onClose} className="block w-full rounded-md px-2.5 py-1.5 text-left text-sm text-muted hover:bg-surface2">Cancel</button>
          </>
        )}
        {drop.phase === 'error' && (
          <div className="px-2.5 py-2 text-sm text-danger">{drop.message}</div>
        )}
      </div>
    </div>,
    document.body,
  )
}

// UploadDialog is the modal that owns a transfer once a destination is picked.
//
// It is deliberately not dismissible by clicking away or pressing Escape: a
// copy that can run for minutes should not vanish because the pointer slipped,
// and the outcome — however it ended — has to be acknowledged.
//
// The two live phases are different in kind, not just in degree:
//   uploading  — bytes on the wire, a real percentage, safely cancellable
//                (the server has not finished parsing the body, so nothing has
//                been written into the container yet).
//   extracting — the body is in; the server is streaming the tar into the
//                container. No percentage to report, and cancelling now would
//                cut the extract halfway and leave a partial destination, so
//                Cancel is withdrawn rather than offered and quietly ignored.
function UploadDialog({ xfer, onCancel, onClose }) {
  const { phase, count, dest, label } = xfer
  const files = `${count} file${count === 1 ? '' : 's'}`
  const pct = xfer.wire > 0 ? Math.min(100, Math.round((xfer.sent / xfer.wire) * 100)) : 0
  const live = phase === 'uploading' || phase === 'extracting'

  const TITLE = {
    uploading: `Copying to ${label}`,
    extracting: `Copying to ${label}`,
    done: 'Transfer complete',
    cancelled: 'Transfer cancelled',
    error: 'Transfer failed',
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl">
        <h3 className="mb-1 text-sm font-semibold">{TITLE[phase]}</h3>
        <p className="mb-4 text-xs text-muted">
          {files}{xfer.total ? ` · ${fmtBytes(xfer.total)}` : ''} → <span className="font-mono text-fg">{dest}</span>
        </p>

        {phase === 'uploading' && (
          <>
            <div className="h-2 w-full overflow-hidden rounded-full bg-surface2">
              <div className="h-full rounded-full bg-primary transition-[width] duration-150" style={{ width: `${pct}%` }} />
            </div>
            <div className="mt-1.5 flex justify-between text-[11px] text-muted">
              <span>{fmtBytes(xfer.sent)} of {fmtBytes(xfer.wire)}</span>
              <span>{pct}%</span>
            </div>
          </>
        )}

        {phase === 'extracting' && (
          <>
            {/* Indeterminate: the server is unpacking and reports no progress.
                Saying "almost done" would be a guess; saying nothing reads as a
                hang. A moving bar with an honest label is the middle. */}
            <div className="h-2 w-full overflow-hidden rounded-full bg-surface2">
              <div className="h-full w-1/3 animate-pulse rounded-full bg-primary" />
            </div>
            <div className="mt-1.5 text-[11px] text-muted">Unpacking on the node…</div>
          </>
        )}

        {phase === 'done' && (
          <div className="flex items-start gap-2 rounded-lg border border-success/30 bg-success/10 px-3 py-2 text-sm text-success">
            <span className="mt-0.5 shrink-0"><Icon.Check size={16} /></span>
            <span>Copied {files} to <span className="font-mono">{dest}</span> on {label}.</span>
          </div>
        )}
        {phase === 'cancelled' && (
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-sm text-warning">
            Cancelled at {pct}%. Nothing was written to {label} — the server discards a transfer it never finished receiving.
          </div>
        )}
        {phase === 'error' && (
          <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{xfer.message}</div>
        )}

        <div className="mt-4 flex justify-end gap-2">
          {phase === 'uploading' && <Button variant="ghost" size="sm" onClick={onCancel}>Cancel</Button>}
          {phase === 'extracting' && <span className="text-[11px] text-muted">Too late to cancel — this would leave a partial copy.</span>}
          {!live && <Button size="sm" onClick={onClose}>OK</Button>}
        </div>
      </div>
    </div>,
    document.body,
  )
}

function ContextMenu({ menu, onClose, actions }) {
  const x = Math.min(menu.x, window.innerWidth - 200)
  const y = Math.min(menu.y, window.innerHeight - 160)
  return createPortal(
    <div className="fixed inset-0 z-50" onClick={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }}>
      <div className="absolute w-52 rounded-lg border bg-surface p-1 shadow-xl" style={{ left: x, top: y }} onClick={(e) => e.stopPropagation()}>
        {actions.map((a, i) =>
          a.sep ? (
            <div key={i} className="my-1 h-px bg-border" />
          ) : (
            <button
              key={i}
              onClick={() => { a.fn(); onClose() }}
              className={`block w-full rounded-md px-2.5 py-1.5 text-left text-sm hover:bg-surface2 ${a.danger ? 'text-danger' : 'text-fg'}`}
            >
              {a.label}
            </button>
          ),
        )}
      </div>
    </div>,
    document.body,
  )
}

const PROPS_KEY = 'dbcanvas-props-layout'
function loadProps() {
  try { return JSON.parse(localStorage.getItem(PROPS_KEY) || '{}') } catch { return {} }
}

function StackProperties({ selected, stackId, nodes, edges, frames, depByNode, patchNode, patchFrame, patchEdge, deleteNode, deleteEdge, deleteFrame, rebuildMongoCluster, deployOpen, deployments, onDeployMinimize }) {
  const selNode = selected?.kind === 'node' ? nodes.find((n) => n.id === selected.id) : null
  const selDep = selNode ? depByNode[selNode.id] : null
  const wide = (selDep && selDep.state === 'running' && (selNode.type === 'intranet' || selNode.type === 'pmm' || selNode.type === 'pxc' || selNode.type === 'proxysql' || selNode.type === 'mysql' || selNode.type === 'ps' || selNode.type === 'innodb' || selNode.type === 'psmdb' || selNode.type === 'psmrs' || selNode.type === 'psm' || selNode.type === 'seaweedfs' || selNode.type === 'patroni' || selNode.type === 'haproxy' || selNode.type === 'pg' || selNode.type === 'repmgr' || selNode.type === 'spock' || selNode.type === 'aio' || selNode.type === 'mariadb' || selNode.type === 'mariadbrepl' || selNode.type === 'mariadbgalera' || selNode.type === 'mysqlce' || selNode.type === 'mysqlcerepl' || selNode.type === 'mysqlceinnodb')) || selected?.kind === 'frame'

  const saved = useRef(loadProps()).current
  const [docked, setDocked] = useState(saved.docked !== false)
  const [width, setWidth] = useState(saved.width || 288)
  const [flt, setFlt] = useState(saved.float || { x: Math.max(20, (typeof window !== 'undefined' ? window.innerWidth : 1200) - 500), y: 96, w: 460, h: 540 })
  const drag = useRef(null)

  // give management tabs room when a running Intranet is selected (docked)
  useEffect(() => { if (wide && docked && width < 440) setWidth(440) }, [wide, docked, width])
  useEffect(() => { try { localStorage.setItem(PROPS_KEY, JSON.stringify({ docked, width, float: flt })) } catch { /* */ } }, [docked, width, flt])

  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'w') setWidth(Math.min(680, Math.max(260, d.w0 + (d.x0 - e.clientX))))
      else if (d.kind === 'move') setFlt((f) => ({ ...f, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) }))
      else if (d.kind === 'wh') setFlt((f) => ({ ...f, w: Math.max(280, d.w0 + (e.clientX - d.x0)), h: Math.max(220, d.h0 + (e.clientY - d.y0)) }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  const Header = ({ move }) => (
    <div
      className="mb-3 flex items-center justify-between"
      onPointerDown={move ? (e) => { if (e.target.closest('button')) return; drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: flt.x, fy: flt.y } } : undefined}
      style={move ? { cursor: 'move' } : undefined}
    >
      <h3 className="text-sm font-semibold">Properties</h3>
      <button onClick={() => setDocked((d) => !d)} title={docked ? 'Detach' : 'Dock'} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
        <Icon.Frame size={14} />
      </button>
    </div>
  )
  const body = <Body selected={selected} stackId={stackId} nodes={nodes} edges={edges} frames={frames} depByNode={depByNode} patchNode={patchNode} patchFrame={patchFrame} patchEdge={patchEdge} deleteNode={deleteNode} deleteEdge={deleteEdge} deleteFrame={deleteFrame} rebuildMongoCluster={rebuildMongoCluster} />

  // Docked deployment console lives at the bottom of this column (under Properties).
  const deployConsole = deployOpen && (
    <DeploymentConsole deployments={deployments} nodes={nodes} onMinimize={onDeployMinimize} inline columnWidth={width} />
  )

  if (docked) {
    return (
      <div className="relative flex shrink-0 flex-col gap-4" style={{ width }}>
        <div
          onPointerDown={(e) => { drag.current = { kind: 'w', x0: e.clientX, w0: width } }}
          className="absolute left-0 top-0 z-10 h-full w-1.5 -translate-x-1 cursor-ew-resize hover:bg-primary"
          title="Drag to resize"
        />
        <div className="min-h-0 flex-1 overflow-auto rounded-xl border bg-surface p-4">
          <Header move={false} />
          {body}
        </div>
        {deployConsole}
      </div>
    )
  }
  return (
    <>
      {createPortal(
        <div className="fixed z-40 flex flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
          style={{ left: flt.x, top: flt.y, width: flt.w, height: flt.h }}>
          <div className="flex-1 overflow-auto p-4">
            <Header move />
            {body}
          </div>
          <div
            onPointerDown={(e) => { drag.current = { kind: 'wh', x0: e.clientX, y0: e.clientY, w0: flt.w, h0: flt.h } }}
            className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize text-muted"
          >
            <svg viewBox="0 0 10 10" className="h-full w-full"><path d="M9 1 L1 9 M9 5 L5 9" stroke="currentColor" fill="none" /></svg>
          </div>
        </div>,
        document.body,
      )}
      {/* Properties is detached, so the docked console can't sit under it — pin it
          to the right-column bottom instead (handled by DeploymentConsole). */}
      {deployOpen && (
        <DeploymentConsole deployments={deployments} nodes={nodes} onMinimize={onDeployMinimize} columnWidth={width} />
      )}
    </>
  )
}

function Body({ selected, stackId, nodes, edges, frames, depByNode, patchNode, patchFrame, patchEdge, deleteNode, deleteEdge, deleteFrame, rebuildMongoCluster }) {
  if (!selected) return <p className="text-sm text-muted">Select a node, link or PXC cluster to edit it. Add an Intranet node from the toolbar to begin.</p>

  if (selected.kind === 'frame') {
    const f = frames.find((x) => x.id === selected.id)
    if (!f) return null
    const frameNodes = nodes.filter((n) => n.frameId === f.id)
    const deployed = frameNodes.some((n) => depByNode[n.id])
    const running = frameNodes.some((n) => depByNode[n.id]?.state === 'running')
    if (f.type === 'proxysql') {
      return <ProxySQLFrameForm frame={f} nodes={nodes} frames={frames} edges={edges} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'mysql') {
      return <MySQLFrameForm frame={f} stackId={stackId} nodes={nodes} frames={frames} edges={edges} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} running={running} />
    }
    if (f.type === 'innodb') {
      return <InnoDBFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'mariadbrepl') {
      return <MariaDBFrameForm frame={f} stackId={stackId} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} running={running} />
    }
    if (f.type === 'mariadbgalera') {
      return <MariaDBGaleraFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'mysqlcerepl') {
      return <MySQLCEFrameForm frame={f} stackId={stackId} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} running={running} />
    }
    if (f.type === 'mysqlceinnodb') {
      return <MySQLCEInnoDBFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'psmdb') {
      return <MongoDBFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} rebuildCluster={rebuildMongoCluster} deployed={deployed} />
    }
    if (f.type === 'psmrs') {
      return <PSMRSFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'patroni') {
      return <PatroniFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'repmgr') {
      return <RepmgrFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'spock') {
      return <SpockFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'valkeycluster') {
      return <ValkeyClusterFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'k3d') {
      return <K3DFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    return <PXCFrameForm frame={f} stackId={stackId} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} running={running} />
  }

  if (selected.kind === 'node') {
    const n = nodes.find((x) => x.id === selected.id)
    if (!n) return null
    const dep = depByNode[n.id]
    const deployed = !!dep

    // Standalone MariaDB / MySQL Community nodes.
    // Once running these show the manager, not the design form — same as `ps`.
    // MySQLManager reads mysqlConfig's keys, and mariadbConfig carries the same
    // JSON tags for every one of them (the two it lacks, dirAuth and vault, are
    // features MariaDB does not offer here and render falsy).
    if (n.type === 'mariadb') {
      if (dep && dep.state === 'running') {
        return <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MariaDBNodeForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} deployed={deployed} />
    }
    if (n.type === 'mysqlce') {
      if (dep && dep.state === 'running') {
        return <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MySQLCENodeForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} deployed={deployed} />
    }
    // Members of the four upstream cluster kinds. Replication members carry a role;
    // Galera and Group Replication members do not (both are multi-master).
    if (n.type === 'mariadbrepl' || n.type === 'mariadbgalera' || n.type === 'mysqlcerepl' || n.type === 'mysqlceinnodb') {
      if (dep && dep.state === 'running') {
        // The InnoDB/GR members record innodbConfig, so they get the manager that
        // understands it (cluster topology, group name, Router RW/RO ports).
        return n.type === 'mysqlceinnodb'
          ? <InnoDBManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
          : <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return (
        <UpstreamMemberForm
          node={n} frame={frames.find((fr) => fr.id === n.frameId)}
          patchNode={patchNode} deleteNode={deleteNode} deployed={deployed}
          roles={n.type === 'mariadbrepl' || n.type === 'mysqlcerepl'}
        />
      )
    }

    // PXC cluster member node.
    if (n.type === 'pxc') {
      if (dep && dep.state === 'running') {
        return <PXCManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PXCNodeForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} nodes={nodes} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // MySQL replication member node.
    if (n.type === 'mysql') {
      if (dep && dep.state === 'running') {
        return <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MySQLMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} nodes={nodes} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // InnoDB Cluster / GR member node.
    if (n.type === 'innodb') {
      if (dep && dep.state === 'running') {
        return <InnoDBManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <InnoDBMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // PS MongoDB sharded-cluster member node.
    if (n.type === 'psmdb') {
      if (dep && dep.state === 'running') {
        return <MongoDBManager stackId={stackId} nodeId={n.id} frameId={n.frameId} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MongoDBMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // PS MongoDB replica-set member node.
    if (n.type === 'psmrs') {
      if (dep && dep.state === 'running') {
        return <MongoDBManager stackId={stackId} nodeId={n.id} frameId={n.frameId} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PSMRSMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // Standalone PS MongoDB node.
    if (n.type === 'psm') {
      if (dep && dep.state === 'running') {
        return <MongoDBManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PSMStandaloneForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }

    // Patroni PostgreSQL cluster member node.
    if (n.type === 'patroni') {
      if (dep && dep.state === 'running') {
        return <PatroniManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PatroniMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // repmgr PostgreSQL cluster member node.
    if (n.type === 'repmgr') {
      if (dep && dep.state === 'running') {
        return <RepmgrManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <RepmgrMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }
    // Spock cluster member node.
    if (n.type === 'spock') {
      if (dep && dep.state === 'running') {
        return <SpockManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <SpockMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }
    // k3s node inside a K3D cluster frame.
    if (n.type === 'k3d') {
      if (dep && dep.state === 'running') {
        return <K3DManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <K3DMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} frameNodes={nodes.filter((x) => x.frameId === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }
    // Valkey cluster member node.
    if (n.type === 'valkeycluster') {
      if (dep && dep.state === 'running') {
        return <ValkeyManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <ValkeyClusterMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // HAProxy node (load balancer for a Patroni cluster).
    if (n.type === 'haproxy') {
      if (dep && dep.state === 'running') {
        return <HAProxyManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <HAProxyForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }

    // Percona Orchestrator node (topology visualization / failure detection). Not
    // linked via a canvas edge — MySQL replication frames point at it through
    // their own "Monitored by (Orchestrator)" picker instead.
    if (n.type === 'orchestrator') {
      if (dep && dep.state === 'running') {
        return <OrchestratorManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <OrchestratorForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }

    // All-in-One node: one container, many database instances. Running → the
    // instance console (start/stop/logs per instance or per cluster).
    if (n.type === 'aio') {
      if (dep && dep.state === 'running') {
        return <AllInOneManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <AllInOneForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }

    const def = NODE_TYPES[n.type] || NODE_TYPES.intranet

    // Deployed + running Intranet → full management console.
    if (dep && dep.state === 'running' && n.type === 'intranet') {
      return <IntranetManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // Samba AD DC singleton node.
    if (n.type === 'sambaad') {
      if (dep && dep.state === 'running') {
        return <SambaManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <SambaForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Deployed + running PMM → PMM management console.
    if (dep && dep.state === 'running' && n.type === 'pmm') {
      return <PMMManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // Deployed + running ProxySQL → ProxySQL management console.
    if (dep && dep.state === 'running' && n.type === 'proxysql') {
      return <ProxySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // ProxySQL (not yet running): a cluster member shows the per-member form; a
    // standalone node shows the full options form.
    if (n.type === 'proxysql') {
      if (n.frameId) {
        return <ProxySQLFrameMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
      }
      return <ProxySQLForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Standalone Percona Server node.
    if (n.type === 'ps') {
      if (dep && dep.state === 'running') {
        return <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PerconaServerForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Standalone PostgreSQL node.
    if (n.type === 'pg') {
      if (dep && dep.state === 'running') {
        return <PGManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PostgreSQLForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // SeaweedFS object-storage node.
    if (n.type === 'seaweedfs') {
      if (dep && dep.state === 'running') {
        return <SeaweedFSManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <SeaweedFSForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Watchtower singleton node (container auto-upgrades for PMM).
    if (n.type === 'watchtower') {
      if (dep && dep.state === 'running') {
        return <WatchtowerManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <WatchtowerForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // OpenBao secrets-manager node (Vault-compatible KMS for Percona encryption).
    if (n.type === 'openbao') {
      if (dep && dep.state === 'running') {
        return <OpenBaoManager dep={dep} stackId={stackId} nodeId={n.id} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <OpenBaoForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Keycloak singleton node (OIDC identity provider).
    if (n.type === 'keycloak') {
      if (dep && dep.state === 'running') {
        return <KeycloakManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <KeycloakForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Ubuntu VNC desktop node.
    if (n.type === 'vnc') {
      if (dep && dep.state === 'running') {
        return <VNCManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <VNCForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Standalone Valkey node.
    if (n.type === 'valkey') {
      if (dep && dep.state === 'running') {
        return <ValkeyManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <ValkeyForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Linux Client node — a bare OS host with no product installed.
    if (n.type === 'linuxclient') {
      if (dep && dep.state === 'running') {
        return <LinuxClientManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <LinuxClientForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Traffic Sim node — the Valkey Traffic Lab live demo app.
    if (n.type === 'trafficsim') {
      if (dep && dep.state === 'running') {
        return <TrafficSimManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <TrafficSimForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Hotel Sim node — the MongoDB Hotel Reservation Lab live demo app.
    if (n.type === 'hotelsim') {
      if (dep && dep.state === 'running') {
        return <HotelSimManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <HotelSimForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Airline Sim node — the MySQL Airline Reservation Lab live demo app.
    if (n.type === 'airlinesim') {
      if (dep && dep.state === 'running') {
        return <AirlineSimManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <AirlineSimForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Car Rental Sim node — the PostgreSQL Car Rental Lab live demo app.
    if (n.type === 'carsim') {
      if (dep && dep.state === 'running') {
        return <CarSimManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <CarSimForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // MarketChaos node — the "Unoptimized MySQL Challenge" stock-exchange demo app.
    if (n.type === 'marketchaos') {
      if (dep && dep.state === 'running') {
        return <MarketChaosManager node={n} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MarketChaosForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Stock Market Sim node — the CRUD + report stock-exchange app. Its form
    // takes stackId because, uniquely, it can test a database connection before
    // the node is deployed.
    if (n.type === 'stocksim') {
      if (dep && dep.state === 'running') {
        return <StockSimManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <StockSimForm node={n} nodes={nodes} frames={frames} edges={edges} stackId={stackId} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    return (
      <div className="space-y-3">
        {dep && (
          <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
            <span className="text-muted">Deployment</span>
            <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
          </div>
        )}
        <Field label="Label">
          <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
        </Field>
        <Field label="Type">
          <input className={`${inputCls} opacity-70`} value={def.label} readOnly />
        </Field>
        <Field label="Operating system" hint={deployed ? 'Locked — the node is deployed.' : 'Locked once the stack is deployed.'}>
          <select
            className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
            value={n.os}
            disabled={deployed}
            onChange={(e) => patchNode(n.id, { os: e.target.value })}
          >
            {def.osOptions.map((o) => (
              <option key={o.id} value={o.id}>{o.label}</option>
            ))}
          </select>
        </Field>
        {n.type === 'pmm' && <PMMOptions n={n} nodes={nodes} patchNode={patchNode} deployed={deployed} />}
        {!deployed && <p className="text-xs text-muted">Management tabs (LDAP, email, certificate, credentials, terminal) appear here after deploy.</p>}
        <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
          <Icon.Trash size={16} /> Delete node
        </Button>
      </div>
    )
  }

  const ed = edges.find((x) => x.id === selected.id)
  if (!ed) return null
  if (ed.type === 'async' || ed.type === 'bidir') {
    return <ReplicationLinkForm ed={ed} nodes={nodes} patchEdge={patchEdge} deleteEdge={deleteEdge} />
  }
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-sm">
        <span className="font-mono text-xs">{ed.from.node}.{ed.from.port}</span>
        <span className="mx-1 text-muted">→</span>
        <span className="font-mono text-xs">{ed.to.node}.{ed.to.port}</span>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteEdge(ed.id)}>
        <Icon.Trash size={16} /> Delete link
      </Button>
    </div>
  )
}

// ReplicationLinkForm edits a cross-cluster replication link: switch between async
// (either direction) and bidirectional, or delete it. Changes take effect on the
// next Deploy (replication is reconciled at deploy time). Options are anchored to a
// stable node pair (sorted ids) so the active choice doesn't jump when reversed.
function ReplicationLinkForm({ ed, nodes, patchEdge, deleteEdge }) {
  const ends = { [ed.from.node]: ed.from, [ed.to.node]: ed.to }
  const [idA, idB] = [ed.from.node, ed.to.node].sort()
  const endA = ends[idA]
  const endB = ends[idB]
  const labelOf = (id) => nodes.find((n) => n.id === id)?.label || id
  const lA = labelOf(idA)
  const lB = labelOf(idB)
  const current = ed.type === 'bidir' ? 'bidir' : (ed.from.node === idA ? 'ab' : 'ba')
  const opts = [
    { key: 'ab', label: `${lA} → ${lB}`, hint: 'async', apply: () => patchEdge(ed.id, { type: 'async', from: endA, to: endB }) },
    { key: 'ba', label: `${lB} → ${lA}`, hint: 'async', apply: () => patchEdge(ed.id, { type: 'async', from: endB, to: endA }) },
    { key: 'bidir', label: `${lA} ↔ ${lB}`, hint: 'bidirectional', apply: () => patchEdge(ed.id, { type: 'bidir' }) },
  ]
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Replication link</span>
        <Badge tone="success">{ed.type === 'bidir' ? 'bidirectional' : 'async'}</Badge>
      </div>
      <p className="text-xs text-muted">
        Cross-cluster replication between two cluster members. The arrow points from source to replica;
        bidirectional makes each a replica of the other. Applied (and reconciled) on the next Deploy.
      </p>
      <div className="space-y-2">
        {opts.map((o) => (
          <button key={o.key} onClick={o.apply}
            className={`flex w-full items-center justify-between rounded-lg border px-3 py-2 text-sm ${current === o.key ? 'border-primary bg-primary/10' : 'hover:border-primary hover:bg-primary/5'}`}>
            <span className="font-mono">{o.label}</span>
            <span className="text-[11px] text-muted">{o.hint}</span>
          </button>
        ))}
      </div>
      {ed.type === 'bidir' && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          Bidirectional replication is multi-writer — avoid writing the same rows on both sides.
        </div>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteEdge(ed.id)}>
        <Icon.Trash size={16} /> Delete replication link
      </Button>
    </div>
  )
}

// ReplicationLinkModal asks for the direction/type when a replication link is drawn
// between two cluster members (PXC or Percona Server, in different frames).
function ReplicationLinkModal({ prompt, nodes, frames, onClose, onChoose }) {
  const { e1, e2 } = prompt
  const node = (id) => nodes.find((n) => n.id === id)
  const n1 = node(e1.node)
  const n2 = node(e2.node)
  const frameLabel = (n) => frames.find((f) => f.id === n?.frameId)?.label || ''
  const l1 = n1?.label || 'node'
  const l2 = n2?.label || 'node'
  const opts = [
    { from: e1, to: e2, mode: 'async', label: `${l1} → ${l2}`, hint: 'async — replica reads from source' },
    { from: e2, to: e1, mode: 'async', label: `${l2} → ${l1}`, hint: 'async — replica reads from source' },
    { from: e1, to: e2, mode: 'bidir', label: `${l1} ↔ ${l2}`, hint: 'bidirectional — each replicates from the other' },
  ]
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">Set up replication</h3>
        <p className="mb-3 text-xs text-muted">
          Between <span className="font-semibold">{l1}</span> ({frameLabel(n1)}) and <span className="font-semibold">{l2}</span> ({frameLabel(n2)}).
          Configured at deploy time (GTID auto-position when both clusters use GTID, else binlog file/position).
        </p>
        <div className="space-y-2">
          {opts.map((o, i) => (
            <button key={i} onClick={() => onChoose(o.from, o.to, o.mode)}
              className="flex w-full items-center justify-between rounded-lg border px-3 py-2 text-left text-sm hover:border-primary hover:bg-primary/10">
              <span className="font-mono">{o.label}</span>
              <span className="ml-2 text-[11px] text-muted">{o.hint}</span>
            </button>
          ))}
        </div>
        <div className="mt-4 flex justify-end">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}
