// Package store owns every SQL statement in Pixelforge. Nothing above it knows
// what a table looks like, and nothing below it knows what a room is for.
package store

// schema is applied on every boot. Every statement is idempotent, so a restart,
// a rollback, or two replicas starting at the same instant are all harmless.
//
// Rooms arrived after the first release, so this also migrates a single-canvas
// installation forward: the v1 tables are read into a room called "main" and
// then left alone rather than dropped, because a migration that destroys the
// only copy of the data is a migration you cannot walk back.
const schema = `
create table if not exists rooms (
    id           bigserial   primary key,
    slug         text        not null unique,
    name         text        not null,
    width        integer     not null,
    height       integer     not null,
    palette      text        not null default 'classic',
    cooldown_ms  integer     not null default 750,
    visibility   text        not null default 'public',
    owner_hash   text        not null default '',
    owner_user   bigint      null,
    paused       boolean     not null default false,
    created_at   timestamptz not null default now(),
    last_active  timestamptz not null default now()
);

create index if not exists rooms_activity_idx   on rooms (last_active desc);
create index if not exists rooms_visibility_idx on rooms (visibility, last_active desc);
create index if not exists rooms_owner_idx      on rooms (owner_user);

create table if not exists room_placements (
    id         bigserial   primary key,
    room_id    bigint      not null,
    room_seq   bigint      not null,
    x          integer     not null,
    y          integer     not null,
    color      smallint    not null,
    uid        text        not null,
    created_at timestamptz not null default now(),
    undone     boolean     not null default false
);

-- Every read of the placement log is "this room, in order, after a point".
create index if not exists room_placements_seq_idx on room_placements (room_id, room_seq);
create index if not exists room_placements_uid_idx on room_placements (room_id, uid);

-- "Who painted this cell, and what was here before?" is a per-cell question, and
-- without this index it is a sequential scan of the room's whole history.
create index if not exists room_placements_cell_idx on room_placements (room_id, x, y, room_seq desc);

create table if not exists room_snapshots (
    room_id    bigint      primary key,
    width      integer     not null,
    height     integer     not null,
    pixels     bytea       not null,
    seq        bigint      not null,
    updated_at timestamptz not null default now()
);

create table if not exists users (
    id           bigserial   primary key,
    username     text        not null,
    username_key text        not null unique,
    pw_hash      text        not null,
    created_at   timestamptz not null default now()
);

create table if not exists bans (
    room_id    bigint      not null,
    uid        text        not null,
    created_at timestamptz not null default now(),
    primary key (room_id, uid)
);

create table if not exists locks (
    id      bigserial primary key,
    room_id bigint    not null,
    x1      integer   not null,
    y1      integer   not null,
    x2      integer   not null,
    y2      integer   not null
);

create index if not exists locks_room_idx on locks (room_id);
`

// migrateV1 folds a pre-rooms installation into a room named "main". It runs
// after schema and is a no-op once the v1 tables are gone or already drained.
//
// The guard is the existence of the old tables plus an empty rooms table: if
// someone has already created rooms, there is nothing to fold in and running
// this would invent a duplicate.
const migrateV1 = `
do $$
declare
    v_room_id  bigint;
    v_has_old  boolean;
    v_has_new  boolean;
begin
    select to_regclass('public.placements') is not null into v_has_old;
    select exists (select 1 from rooms) into v_has_new;

    if not v_has_old or v_has_new then
        return;
    end if;

    insert into rooms (slug, name, width, height, palette, cooldown_ms, visibility)
    select 'main', 'Main canvas',
           coalesce((select width  from snapshots where id = 1), 256),
           coalesce((select height from snapshots where id = 1), 256),
           'classic', 750, 'public'
    returning id into v_room_id;

    insert into room_placements (room_id, room_seq, x, y, color, uid, created_at)
    select v_room_id, seq, x, y, color, uid, created_at
      from placements
     order by seq;

    insert into room_snapshots (room_id, width, height, pixels, seq, updated_at)
    select v_room_id, width, height, pixels, seq, updated_at
      from snapshots
     where id = 1
    on conflict (room_id) do nothing;
end $$;
`
