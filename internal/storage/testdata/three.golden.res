# Written by clowderd; changes are overwritten.
resource clw-shop-pgdata-b582c809 {
  options { quorum majority; on-no-quorum suspend-io; auto-promote no; }
  net { protocol C; after-sb-0pri discard-zero-changes;
        after-sb-1pri discard-secondary; after-sb-2pri disconnect; }
  disk { c-max-rate 30M; c-plan-ahead 20; }
  device minor 3;
  meta-disk internal;
  on node0 { node-id 0; address 10.77.0.1:10003; disk /dev/vg-fast/clw-shop-pgdata-b582c809-g1; }
  on node1 { node-id 1; address 10.77.0.2:10003; disk /dev/vg-fast/clw-shop-pgdata-b582c809-g1; }
  on node2 { node-id 2; address 10.77.0.3:10003; disk none; }
  connection-mesh { hosts node0 node1 node2; }
}
