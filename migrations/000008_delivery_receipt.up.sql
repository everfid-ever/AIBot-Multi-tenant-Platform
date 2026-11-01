BEGIN;
CREATE TABLE public.delivery_receipt (
 tenant_id text NOT NULL, delivery_key text NOT NULL,
 segment_no integer NOT NULL CHECK(segment_no>=0),
 client_request_id text NOT NULL CHECK(length(btrim(client_request_id))>0),
 provider_message_id text NOT NULL CHECK(length(btrim(provider_message_id)) BETWEEN 1 AND 1024),
 plan_digest text NOT NULL CHECK(plan_digest ~ '^[0-9a-f]{64}$'),
 route_digest text NOT NULL CHECK(route_digest ~ '^[0-9a-f]{64}$'),
 segment_digest text NOT NULL CHECK(segment_digest ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,delivery_key,segment_no),
 FOREIGN KEY(tenant_id,delivery_key,segment_no) REFERENCES public.delivery_ledger(tenant_id,delivery_key,segment_no)
);
COMMIT;
