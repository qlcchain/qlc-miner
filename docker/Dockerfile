# Build gqlc-miner in a stock Go builder container
FROM qlcchain/go-qlc-builder:1.13.1 as builder

ARG BUILD_ACT=build

COPY . /qlcchain/qlc-miner
RUN cd /qlcchain/qlc-miner && make clean ${BUILD_ACT}

# Pull gqlc into a second stage deploy alpine container
FROM alpine:3.10.2

COPY --from=builder /qlcchain/qlc-miner/build/gqlc-miner .

# EXPOSE 9734 9735 9736

ENTRYPOINT [ "/gqlc-miner"]
