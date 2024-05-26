import { toHexString } from "@chainsafe/ssz";
import { capella, phase0, ssz } from "@lodestar/types";
import { AxiosInstance } from "axios";

type BeaconId = number | string | Uint8Array | bigint;

export const ROUTES = {
  getBlock: "/eth/v2/beacon/blocks/{block_id}",
  getGenesis: "/eth/v1/beacon/genesis",
  getHeader: "/eth/v1/beacon/headers/{block_id}",
  getBeaconState: "/eth/v2/debug/beacon/states/{state_id}",
  getSyncStatus: "/eth/v1/node/syncing",
};

const config = {
  secondsPerSlot: 12,
  slotsPerEpoch: 32,
  epochsPerPeriod: 256,
  capellaForkEpoch: 194048,
  capellaForkSlot: 194048 * 32,
};

export class ConsensusClient {
  client: AxiosInstance;
  slotsPerEpoch: number;
  slotsPerPeriod: number;
  SLOTS_PER_HISTORICAL_ROOT = 8192;

  constructor(
    axiosClient: AxiosInstance,
    slotsPerEpoch: number,
    slotsPerPeriod: number
  ) {
    this.client = axiosClient;
    this.slotsPerEpoch = slotsPerEpoch;
    this.slotsPerPeriod = slotsPerPeriod;
  }

  toStringFromBeaconId(identifier: any) {
    if (identifier instanceof Uint8Array) {
      return toHexString(identifier);
    }
    return identifier.toString();
  }

  async getHeader(
    blockIdentifier: BeaconId
  ): Promise<phase0.BeaconBlockHeader> {
    const id = this.toStringFromBeaconId(blockIdentifier);
    const response = await this.client.get(
      ROUTES.getHeader.replace("{block_id}", id)
    );
    const header = ssz.phase0.BeaconBlockHeader.fromJson(
      response.data.data.header.message
    );
    return header;
  }

  async getSSZ(blockIdentifier: BeaconId) {
    let slot: number;
    if (typeof blockIdentifier === "number") {
      slot = blockIdentifier;
    } else {
      const header = await this.getHeader(blockIdentifier);
      slot = header.slot;
    }
    if (slot < config.capellaForkSlot) {
      return ssz.bellatrix;
    } else {
      return ssz.capella;
    }
  }

  async getBlock(blockIdentifier: BeaconId): Promise<capella.BeaconBlock> {
    const ssz = await this.getSSZ(blockIdentifier);
    const id = this.toStringFromBeaconId(blockIdentifier);
    const response = await this.client.get(
      ROUTES.getBlock.replace("{block_id}", id)
    );
    const block = ssz.BeaconBlock.fromJson(
      response.data.data.message
    ) as capella.BeaconBlock;
    return block;
  }
}
