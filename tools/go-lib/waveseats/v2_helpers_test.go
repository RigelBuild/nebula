// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

const v2TestStamp = "2026-11-04T12:34:56.000Z"

func v2TestSeat(name string) SeatEntryV2 {
	return SeatEntryV2{
		Name: name, SeatRevision: 1, CWD: "/work/" + name, Tab: name,
		State: SeatV2Active, RestartGeneration: 0, UpdatedAt: v2TestStamp,
		Profile: "worker", Role: "worker", RoleTreeRevision: 0,
		Key:       KeySettings{Routing: map[Provider]any{}, Limits: KeyLimits{}, PolicyID: "policy-a"},
		Workspace: "/work/" + name, Persona: name,
	}
}

func v2TestList(revision uint64, names ...string) SeatListV2 {
	seats := make([]SeatEntryV2, 0, len(names))
	for _, name := range names {
		seats = append(seats, v2TestSeat(name))
	}
	return SeatListV2{Version: 2, Revision: revision, Seats: seats}
}
