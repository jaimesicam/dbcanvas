# Stalk Summary

Stalk Summary turns a **pt-stalk** archive from a MySQL-family server into timeline charts,
verdicts and configuration advice. It also compares captures, for example one taken before
a change and one taken after.

Open it from the sidebar (**Stalk Summary**).

![Stalk Summary: verdicts and charts from a pt-stalk capture](screenshots/stalk-summary.png)

## Get a capture

- **From a node in a stack.** Open a MySQL-family node (Percona Server, PXC, MySQL, MariaDB),
  go to its **Diagnostics** tab and click **Start capture**. The capture runs `pt-summary`,
  `pt-mysql-summary` and about 90 seconds of `pt-stalk` sampling. When it finishes, click
  **Stalk Summary** to open it on this page, or **Download (.tar.gz)** to keep the file.
- **From a node, on this page.** Choose the node under **…or use a node's collected
  capture** and click **Analyze pt-stalk**. This uses the node's most recent capture.
- **Upload.** Drop a pt-stalk `.tar.gz` on the page, or click to choose one (up to 128 MB).
  Archives from any server work, and missing files are skipped instead of failing the
  read.

Every finished capture from a node is kept under the time it was taken, in **Kept pt-stalk
captures**. Kept captures remain after the node or stack is deleted. Each one has **Open**,
**Download** and **Delete**.

## What it shows

- **Summary**: CPU, RAM, version, uptime, kernel, buffer pool size, `flush_method`, redo
  size, `sync_binlog` and `flush_log_at_trx_commit`, plus a flag if a deadlock was
  detected.
- **Verdicts**: conclusions drawn from the measurements, each with *What this measures* and
  *What to do*. Examples are buffer pool sizing, whether buffer pool misses reach the disk,
  redo log headroom, rows read without an index, and what the limiting resource is.
- **Configuration**: settings to change, each with **Now** and **Change to**.
- **Charts**:

  | Group | Charts |
  | --- | --- |
  | Operating system | CPU busy (overall and per CPU), Memory, Swap used |
  | Disk | Utilization, throughput, IOPS and latency (overall and per device) |
  | Network | MySQL network throughput, connection states, socket backlog, TCP retransmits |
  | MySQL / InnoDB | Buffer pool pages and reads, InnoDB I/O against device I/O, durability cost (fsyncs/s), rows scanned without index, history list length, checkpoint age, replication lag, Galera flow control and receive queue, row-lock waits, threads, query throughput, row operations, temp tables on disk, slow queries, aborted connections, thread states |

- **Tables** (click a column to sort): statements by rows examined, MySQL processlist
  (consolidated per thread and query), lock waits (who is blocking whom), metadata locks,
  open transactions, InnoDB transactions per session, sockets with a sustained backlog,
  notable error-log lines, and the latest detected deadlock.

## Compare captures

- **Head to head.** In **Kept pt-stalk captures**, tick two or more captures and click
  **Compare N captures**. The first one ticked is the baseline. The result lists the
  settings that differ, every measurement side by side with its change, and verdicts such
  as *Did throughput change?*, *Did the buffer pool change help?* and *What got worse*.
- **Against a pinned capture.** With a capture open, click **Pin as baseline to compare**,
  then open another capture. The **Comparison** card shows settings that differ,
  measurements and verdicts that changed.

---

See also: [Log Summary](LOG_SUMMARY.md) · [FTDC Summary](FTDC_SUMMARY.md) · [Packet Inspector](PACKET_INSPECTOR.md)
