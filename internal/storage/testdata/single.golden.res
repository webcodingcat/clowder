# Written by clowderd; changes are overwritten.
resource clw-shop-pgdata-b582c809 {
  options { quorum majority; on-no-quorum suspend-io; auto-promote no; }
  net { protocol C; after-sb-0pri discard-zero-changes;
        after-sb-1pri discard-secondary; after-sb-2pri disconnect; }
  disk { c-max-rate 50M; c-plan-ahead 20; }
  device minor 3;
  meta-disk internal;
  on node0 { node-id 0; address 10.77.0.1:10003; disk /dev/vg-fast/clw-shop-pgdata-b582c809-g1; }
}
