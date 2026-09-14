# syntax=docker/dockerfile:1

# ---- build the shaded jar ----
# Eclipse Temurin, not Oracle JDK: Temurin is GPL-2.0 WITH Classpath-exception-2.0,
# which is what lets this GPL-3.0 program ship on it. See NOTICE.
FROM maven:3.9-eclipse-temurin-21 AS build
WORKDIR /src
# Dependencies resolve in their own layer so a source edit does not re-download
# the drivers every build.
COPY pom.xml ./
RUN mvn -B -q dependency:go-offline
COPY src ./src
COPY NOTICE ./
RUN mvn -B -q package -DskipTests

# ---- runtime ----
FROM eclipse-temurin:21-jre
COPY --from=build /src/target/ledgersim.jar /opt/ledgersim/ledgersim.jar
COPY NOTICE /opt/ledgersim/NOTICE

# dbcanvas waits for readiness by Exec'ing a check inside the container, the same
# way it does for the Go sims (`/ledgersim -healthcheck`). This wrapper keeps that
# contract identical across simulators regardless of what is underneath it.
RUN printf '#!/bin/sh\nexec java $JAVA_OPTS -jar /opt/ledgersim/ledgersim.jar "$@"\n' > /ledgersim \
 && chmod +x /ledgersim

# Container-aware heap by percentage rather than a fixed -Xmx: a Ledger Sim node
# may be given anything from 256 MB to several GB, and a hard-coded heap is either
# a waste or an OOM depending on which.
ENV JAVA_OPTS="-XX:MaxRAMPercentage=70 -XX:+ExitOnOutOfMemoryError"
ENV PORT=8094
EXPOSE 8094
ENTRYPOINT ["/ledgersim"]
