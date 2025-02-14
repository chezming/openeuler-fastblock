/* Copyright (c) 2023-2024 ChinaUnicom
 * fastblock is licensed under Mulan PSL v2.
 * You can use this software according to the terms and conditions of the Mulan PSL v2.
 * You may obtain a copy of Mulan PSL v2 at:
 *          http://license.coscl.org.cn/MulanPSL2
 * THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
 * EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
 * MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
 * See the Mulan PSL v2 for more details.
 */
package osd

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"monitor/config"
	"monitor/etcdapi"
	"monitor/log"
	"monitor/msg"
)

const (
	// MinPoolNum defines min pool number, from 1.
	MinPoolNum = 1
	// MinPGID defines min PG ID, 0 or 1.
	MinPGID = uint64(0)
)

// PGID is the PG ID in each pool. Excluding pool number.
// TODO: now use string in etcd.
type PGID uint64

// PGsConfig 7/pools/$PGID, output.
// put pgmap in on key/value pair so we cant version it golablly
// Example: {"version":123,"pgmap":{"1_1":[1,2,3],"1_2":[2,3,4]}}
//
//	means pool 1, pg 1 osd [1,2,3], pg 2 osd [2,3,4]
//
// TODO: for each pool or any incremental update?
type PoolPGsConfig struct {
	Version int64               `json:"version,omitempty"`
	PgMap   map[string]PGConfig `json:"pgmap,omitempty"`
}

// PGConfig for each pg in /config/pgmap, output.
// Example: {"1":[1,2,3]}
// TODO: use OSDID, not string. It requires parse change.
// type PGConfig []int
type PGConfig struct {
	Version int64 `json:"version,omitempty"`
	OsdList []int `json:"osdlist,omitempty"`
}

// PoolID defines pool ID.
type PoolID int32

// FailureDomain defined for crush.
type FailureDomain string

func (domain FailureDomain) String() string {
	return string(domain)
}

var validFailureDomainMap = map[string]bool{"osd": true, "host": true, "rack": true, "root": true}

func (domain FailureDomain) IsValid() bool {
	if valid := validFailureDomainMap[domain.String()]; valid {
		return true
	}

	return false
}

// PoolConfig for each pool in /config/pools.
// use string for id too
type PoolConfig struct {
	Poolid        int           `json:"poolid,omitempty"`
	Name          string        `json:"name,omitempty"`
	PGSize        int           `json:"pg_size,omitempty"`
	PGCount       int           `json:"pg_count,omitempty"`
	FailureDomain string        `json:"failure_domain,omitempty"`
	Root          string        `json:"root,omitempty"`
	PoolPgMap     PoolPGsConfig `json:"poolpgmap,omitempty"`
}

// 已经包含了pg的分配表
var AllPools map[PoolID]*PoolConfig
var lastSeenPoolId int32
var osdmapVersion int64

// findUsablePoolId finds the first available pool id
// we don't reuse pool ids, so it always increaing
// poll deletions are rare, we are safe to use int32.
func findUsablePoolId() PoolID {
	return PoolID(lastSeenPoolId + 1)
}

// Example: /config/pools/pool/1 '{"poolid": 1, "name":"testpool","pg_size":3, "pg_count":256,"failure_domain":"host"}'
// Example: /config/pools/pool/2 '{"poolid": 2, "name":"testpool2","pg_size":3, "pg_count":256,"failure_domain":"host"}'
// poolid is somewhat redudant?
// call it on start
func LoadPoolConfig(ctx context.Context, client *etcdapi.EtcdClient) (err error) {
	lastSeenPoolId = 0
	AllPools = make(map[PoolID]*PoolConfig)

	kvs, getErr := client.GetWithPrefix(ctx, config.ConfigPoolsKeyPrefix)
	if getErr != nil {
		log.Error(ctx, getErr)
		return getErr
	}

	if len(kvs) == 0 {
		log.Info(ctx, "no pool created yet")
		return nil
	}

	for _, kv := range kvs {
		log.Info(ctx, "pool key:", kv.Key, ", value:", string(kv.Value))

		k := string(kv.Key)
		poolID, err := strconv.Atoi(strings.TrimPrefix(k, config.ConfigPoolsKeyPrefix))
		if err != nil {
			log.Info(ctx, "failed to get poolId")
			continue
		}

		var poolConfig PoolConfig
		if jerr := json.Unmarshal([]byte(kv.Value), &poolConfig); jerr != nil {
			log.Error(ctx, jerr, string(kv.Key), string(kv.Value))
			return jerr
		}

		AllPools[PoolID(poolID)] = &poolConfig
		if lastSeenPoolId < int32(poolID) {
			lastSeenPoolId = int32(poolID)
		}

		log.Debug(ctx, "pool", poolID)
	}

	log.Info(ctx, "loadPoolConfig done")
	for k, v := range AllPools {
		log.Info(ctx, k, v)
	}

	return nil
}

func ProcessCreatePoolMessage(ctx context.Context, client *etcdapi.EtcdClient, name string, size int, pc int, fd string, root string) (int32, error) {
	for _, pc := range AllPools {
		if name == pc.Name {
			return -1, fmt.Errorf("the pool name is already occupied by other pools")
		}
	}

	pid := findUsablePoolId()

	//pgs of the pool is created by RecheckPgs
	ppgc := PoolPGsConfig{Version: 0, PgMap: make(map[string]PGConfig)}
	poolConf := &PoolConfig{
		Poolid:        int(pid),
		Name:          name,
		PGSize:        size,
		PGCount:       pc,
		FailureDomain: fd,
		Root:          root,
		PoolPgMap:     ppgc,
	}

	pc_buf, err := json.Marshal(poolConf)
	if err != nil {
		log.Error(ctx, err)
		return -1, err
	}

	key := fmt.Sprintf("%s%d", config.ConfigPoolsKeyPrefix, pid)

	err = client.Put(ctx, key, string(pc_buf))
	if err != nil {
		log.Error(ctx, err)
		return -1, err
	}

	if lastSeenPoolId < int32(pid) {
		lastSeenPoolId = int32(pid)
	}
	AllPools[PoolID(pid)] = poolConf

	log.Info(ctx, "successfully put to ectd for newly created pool", pid)
	return int32(pid), nil
}

func ProcessListPoolsMessage(ctx context.Context) ([]*msg.Poolinfo, error) {
	pis := make([]*msg.Poolinfo, 0)
	if len(AllPools) == 0 {
		return nil, nil
	}

	for _, pool := range AllPools {
		pi := &msg.Poolinfo{}
		pi.Failuredomain = pool.FailureDomain
		pi.Name = pool.Name
		pi.Pgcount = int32(pool.PGCount)
		pi.Root = pool.Root
		pi.Pgsize = int32(pool.PGSize)
		pi.Poolid = int32(pool.Poolid)
		pis = append(pis, pi)
	}

	return pis, nil
}

func ProcessGetPgMapMessage(ctx context.Context, pvs map[int32]int64) (*msg.GetPgMapResponse, error) {
	log.Info(ctx, "ProcessGetPgMapMessage", pvs)

	if len(AllPools) == 0 {
		log.Info(ctx, "no pool created yet")
		return nil, fmt.Errorf("no pool created yet")
	}

	gpmr := &msg.GetPgMapResponse{
		Errorcode:          make(map[int32]msg.GetPgMapErrorCode),
		PoolidPgmapversion: make(map[int32]int64),
		Pgs:                make(map[int32]*msg.PGInfos),
	}

	addAll := false

	//if user don't specify poolid, we will give it all pools because on start, they don't know anything
	if len(pvs) == 0 {
		addAll = true
	}
	//we return whole map
	for pid, ppc := range AllPools {
		log.Info(ctx, pid, *ppc)
		if addAll {
			log.Info(ctx, "add all")
			pginfos := &msg.PGInfos{
				Pi: make([]*msg.PGInfo, 0),
			}
			for pgid, pc := range ppc.PoolPgMap.PgMap {
				pgidToi, _ := strconv.Atoi(pgid)
				var osdlist []int32
				for _, oid := range pc.OsdList {
					osdlist = append(osdlist, int32(oid))
				}
				pi := &msg.PGInfo{
					Pgid:    int32(pgidToi),
					Version: pc.Version,
					Osdid:   osdlist,
				}
				pginfos.Pi = append(pginfos.Pi, pi)
				gpmr.Pgs[int32(pid)] = pginfos
				gpmr.Errorcode[int32(pid)] = msg.GetPgMapErrorCode_pgMapGetOk
				gpmr.PoolidPgmapversion[int32(pid)] = ppc.PoolPgMap.Version
			}
		} else {
			log.Info(ctx, "user specified pools")
			// case the client specified poolid and versions
			for clientPid, clientVersion := range pvs {
				if clientPid == int32(pid) {
					if clientVersion > ppc.PoolPgMap.Version {
						gpmr.Errorcode[int32(pid)] = msg.GetPgMapErrorCode_pgMapclientVersionHigher
						continue
					} else if clientVersion == ppc.PoolPgMap.Version {
						// we have exactly the same version, just return it
						gpmr.Errorcode[int32(pid)] = msg.GetPgMapErrorCode_PgMapSameVersion
					}
				}
				// for onther cases:
				// 1. client didn't specify poolid, but we have ,we should return it to them
				// 2. client specified poolid, but with lower version, we return it to them
				// (TODO)3. client specified poolid, but  we don't have, we tell them the pool is deleted
				pginfos := &msg.PGInfos{
					Pi: make([]*msg.PGInfo, 0),
				}
				for pgid, pc := range ppc.PoolPgMap.PgMap {
					pgidToi, _ := strconv.Atoi(pgid)
					var osdlist []int32
					for _, oid := range pc.OsdList {
						osdlist = append(osdlist, int32(oid))
					}
					pi := &msg.PGInfo{
						Pgid:    int32(pgidToi),
						Version: pc.Version,
						Osdid:   osdlist,
					}
					pginfos.Pi = append(pginfos.Pi, pi)
					gpmr.Pgs[int32(pid)] = pginfos
					gpmr.Errorcode[int32(pid)] = msg.GetPgMapErrorCode_pgMapGetOk
					gpmr.PoolidPgmapversion[int32(pid)] = ppc.PoolPgMap.Version
				}
			}
		}
	}
	log.Info(ctx, "ProcessGetPgMapMessage done, got pools: ", len(gpmr.PoolidPgmapversion), gpmr)

	return gpmr, nil
}

func ProcessDeletePoolMessage(ctx context.Context, client *etcdapi.EtcdClient, name string) error {
	found := false
	pid := -1
	for _, pc := range AllPools {
		if name == pc.Name {
			pid = pc.Poolid
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("the pool is not found")
	}

	key := fmt.Sprintf("%s%d", config.ConfigPoolsKeyPrefix, pid)

	err := client.Delete(ctx, key)
	if err != nil {
		log.Error(ctx, err)
		return err
	}

	// remove the pool id from the map
	delete(AllPools, PoolID(pid))

	log.Info(ctx, "successfully deleted pool from etcd", pid)
	return nil
}
