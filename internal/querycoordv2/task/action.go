// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package task

import (
	"reflect"

	"github.com/samber/lo"
	"go.uber.org/atomic"

	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	"github.com/milvus-io/milvus/pkg/util/funcutil"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

type ActionType int32

const (
	ActionTypeGrow ActionType = iota + 1
	ActionTypeReduce
	ActionTypeUpdate
)

var ActionTypeName = map[ActionType]string{
	ActionTypeGrow:   "Grow",
	ActionTypeReduce: "Reduce",
	ActionTypeUpdate: "Update",
}

func (t ActionType) String() string {
	return ActionTypeName[t]
}

type Action interface {
	Node() int64
	Type() ActionType
	IsFinished(distMgr *meta.DistributionManager) bool
}

type BaseAction struct {
	nodeID typeutil.UniqueID
	typ    ActionType
	shard  string
}

func NewBaseAction(nodeID typeutil.UniqueID, typ ActionType, shard string) *BaseAction {
	return &BaseAction{
		nodeID: nodeID,
		typ:    typ,
		shard:  shard,
	}
}

func (action *BaseAction) Node() int64 {
	return action.nodeID
}

func (action *BaseAction) Type() ActionType {
	return action.typ
}

func (action *BaseAction) Shard() string {
	return action.shard
}

type SegmentAction struct {
	*BaseAction

	segmentID typeutil.UniqueID
	scope     querypb.DataScope

	rpcReturned atomic.Bool
}

func NewSegmentAction(nodeID typeutil.UniqueID, typ ActionType, shard string, segmentID typeutil.UniqueID) *SegmentAction {
	return NewSegmentActionWithScope(nodeID, typ, shard, segmentID, querypb.DataScope_All)
}

func NewSegmentActionWithScope(nodeID typeutil.UniqueID, typ ActionType, shard string, segmentID typeutil.UniqueID, scope querypb.DataScope) *SegmentAction {
	base := NewBaseAction(nodeID, typ, shard)
	return &SegmentAction{
		BaseAction:  base,
		segmentID:   segmentID,
		scope:       scope,
		rpcReturned: *atomic.NewBool(false),
	}
}

func (action *SegmentAction) SegmentID() typeutil.UniqueID {
	return action.segmentID
}

func (action *SegmentAction) Scope() querypb.DataScope {
	return action.scope
}

func (action *SegmentAction) IsFinished(distMgr *meta.DistributionManager) bool {
	if action.Type() == ActionTypeGrow {
		views := distMgr.LeaderViewManager.GetByFilter(meta.WithSegment2LeaderView(action.segmentID, false))
		nodeSegmentDist := distMgr.SegmentDistManager.GetSegmentDist(action.SegmentID())
		return len(views) > 0 &&
			lo.Contains(nodeSegmentDist, action.Node()) &&
			action.rpcReturned.Load()
	} else if action.Type() == ActionTypeReduce {
		// FIXME: Now shard leader's segment view is a map of segment ID to node ID,
		// loading segment replaces the node ID with the new one,
		// which confuses the condition of finishing,
		// the leader should return a map of segment ID to list of nodes,
		// now, we just always commit the release task to executor once.
		// NOTE: DO NOT create a task containing release action and the action is not the last action
		sealed := distMgr.SegmentDistManager.GetByFilter(meta.WithNodeID(action.Node()))
		views := distMgr.LeaderViewManager.GetByFilter(meta.WithNodeID2LeaderView(action.Node()))
		growing := lo.FlatMap(views, func(view *meta.LeaderView, _ int) []int64 {
			return lo.Keys(view.GrowingSegments)
		})
		segments := make([]int64, 0, len(sealed)+len(growing))
		for _, segment := range sealed {
			segments = append(segments, segment.GetID())
		}
		segments = append(segments, growing...)
		if !funcutil.SliceContain(segments, action.SegmentID()) {
			return true
		}
		return action.rpcReturned.Load()
	} else if action.Type() == ActionTypeUpdate {
		return action.rpcReturned.Load()
	}

	return true
}

type ChannelAction struct {
	*BaseAction
}

func NewChannelAction(nodeID typeutil.UniqueID, typ ActionType, channelName string) *ChannelAction {
	return &ChannelAction{
		BaseAction: NewBaseAction(nodeID, typ, channelName),
	}
}

func (action *ChannelAction) ChannelName() string {
	return action.shard
}

func (action *ChannelAction) IsFinished(distMgr *meta.DistributionManager) bool {
	views := distMgr.LeaderViewManager.GetByFilter(meta.WithChannelName2LeaderView(action.ChannelName()))
	_, hasNode := lo.Find(views, func(v *meta.LeaderView) bool {
		return v.ID == action.Node()
	})
	isGrow := action.Type() == ActionTypeGrow

	return hasNode == isGrow
}

type LeaderAction struct {
	*BaseAction

	leaderID  typeutil.UniqueID
	segmentID typeutil.UniqueID
	version   typeutil.UniqueID // segment load ts, 0 means not set

	partStatsVersions map[int64]int64
	rpcReturned       atomic.Bool
}

func NewLeaderAction(leaderID, workerID typeutil.UniqueID, typ ActionType, shard string, segmentID typeutil.UniqueID, version typeutil.UniqueID) *LeaderAction {
	action := &LeaderAction{
		BaseAction: NewBaseAction(workerID, typ, shard),

		leaderID:  leaderID,
		segmentID: segmentID,
		version:   version,
	}
	action.rpcReturned.Store(false)
	return action
}

func NewLeaderUpdatePartStatsAction(leaderID, workerID typeutil.UniqueID, typ ActionType, shard string, partStatsVersions map[int64]int64) *LeaderAction {
	action := &LeaderAction{
		BaseAction:        NewBaseAction(workerID, typ, shard),
		leaderID:          leaderID,
		partStatsVersions: partStatsVersions,
	}
	action.rpcReturned.Store(false)
	return action
}

func (action *LeaderAction) SegmentID() typeutil.UniqueID {
	return action.segmentID
}

func (action *LeaderAction) Version() typeutil.UniqueID {
	return action.version
}

func (action *LeaderAction) GetLeaderID() typeutil.UniqueID {
	return action.leaderID
}

func (action *LeaderAction) IsFinished(distMgr *meta.DistributionManager) bool {
	views := distMgr.LeaderViewManager.GetByFilter(meta.WithNodeID2LeaderView(action.leaderID), meta.WithChannelName2LeaderView(action.Shard()))
	if len(views) == 0 {
		return false
	}
	view := lo.MaxBy(views, func(v1 *meta.LeaderView, v2 *meta.LeaderView) bool {
		return v1.Version > v2.Version
	})
	dist := view.Segments[action.SegmentID()]
	switch action.Type() {
	case ActionTypeGrow:
		return action.rpcReturned.Load() && dist != nil && dist.NodeID == action.Node()
	case ActionTypeReduce:
		return action.rpcReturned.Load() && (dist == nil || dist.NodeID != action.Node())
	case ActionTypeUpdate:
		return action.rpcReturned.Load() && (dist != nil && reflect.DeepEqual(action.partStatsVersions, view.PartitionStatsVersions))
	}
	return false
}
