/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package multichain

import (
	"fmt"

	"github.com/hyperledger/fabric/common/config"
	"github.com/hyperledger/fabric/common/configtx"
	configtxapi "github.com/hyperledger/fabric/common/configtx/api"
	"github.com/hyperledger/fabric/orderer/ledger"
	cb "github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/utils"
	"github.com/op/go-logging"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/crypto"
)

var logger = logging.MustGetLogger("orderer/multichain")

const (
	msgVersion = int32(0)
	epoch      = 0
)

// Manager coordinates the creation and access of chains
type Manager interface {
	// GetChain retrieves the chain support for a chain (and whether it exists)
	GetChain(chainID string) (ChainSupport, bool)

	// SystemChannelID returns the channel ID for the system channel
	SystemChannelID() string

	// NewChannelConfig returns a bare bones configuration ready for channel
	// creation request to be applied on top of it
	NewChannelConfig(envConfigUpdate *cb.Envelope) (configtxapi.Manager, error)
}

type configResources struct {
	configtxapi.Manager
}

func (cr *configResources) SharedConfig() config.Orderer {
	return cr.OrdererConfig()
}

type ledgerResources struct {
	*configResources
	ledger ledger.ReadWriter
}

type multiLedger struct {
	chains          map[string]*chainSupport
	consenters      map[string]Consenter
	ledgerFactory   ledger.Factory
	signer          crypto.LocalSigner
	systemChannelID string
	systemChannel   *chainSupport
}

func getConfigTx(reader ledger.Reader) *cb.Envelope {
	lastBlock := ledger.GetBlock(reader, reader.Height()-1)
	index, err := utils.GetLastConfigIndexFromBlock(lastBlock)
	if err != nil {
		logger.Panicf("Chain did not have appropriately encoded last config in its latest block: %s", err)
	}
	configBlock := ledger.GetBlock(reader, index)
	if configBlock == nil {
		logger.Panicf("Config block does not exist")
	}

	return utils.ExtractEnvelopeOrPanic(configBlock, 0)
}

// NewManagerImpl produces an instance of a Manager
func NewManagerImpl(ledgerFactory ledger.Factory, consenters map[string]Consenter, signer crypto.LocalSigner) Manager {
	ml := &multiLedger{
		chains:        make(map[string]*chainSupport),
		ledgerFactory: ledgerFactory,
		consenters:    consenters,
		signer:        signer,
	}

	existingChains := ledgerFactory.ChainIDs()
	for _, chainID := range existingChains {
		rl, err := ledgerFactory.GetOrCreate(chainID)
		if err != nil {
			logger.Panicf("Ledger factory reported chainID %s but could not retrieve it: %s", chainID, err)
		}
		configTx := getConfigTx(rl)
		if configTx == nil {
			logger.Panicf("Could not find config transaction for chain %s", chainID)
		}
		ledgerResources := ml.newLedgerResources(configTx)
		chainID := ledgerResources.ChainID()

		if _, ok := ledgerResources.ConsortiumsConfig(); ok {
			if ml.systemChannelID != "" {
				logger.Panicf("There appear to be two system chains %s and %s", ml.systemChannelID, chainID)
			}
			chain := newChainSupport(createSystemChainFilters(ml, ledgerResources),
				ledgerResources,
				consenters,
				signer)
			logger.Infof("Starting with system channel %s and orderer type %s", chainID, chain.SharedConfig().ConsensusType())
			ml.chains[string(chainID)] = chain
			ml.systemChannelID = chainID
			ml.systemChannel = chain
			// We delay starting this chain, as it might try to copy and replace the chains map via newChain before the map is fully built
			defer chain.start()
		} else {
			logger.Debugf("Starting chain: %s", chainID)
			chain := newChainSupport(createStandardFilters(ledgerResources),
				ledgerResources,
				consenters,
				signer)
			ml.chains[string(chainID)] = chain
			chain.start()
		}

	}

	if ml.systemChannelID == "" {
		logger.Panicf("No system chain found")
	}

	return ml
}

func (ml *multiLedger) SystemChannelID() string {
	return ml.systemChannelID
}

// GetChain retrieves the chain support for a chain (and whether it exists)
func (ml *multiLedger) GetChain(chainID string) (ChainSupport, bool) {
	cs, ok := ml.chains[chainID]
	return cs, ok
}

func (ml *multiLedger) newLedgerResources(configTx *cb.Envelope) *ledgerResources {
	initializer := configtx.NewInitializer()
	configManager, err := configtx.NewManagerImpl(configTx, initializer, nil)
	if err != nil {
		logger.Panicf("Error creating configtx manager and handlers: %s", err)
	}

	chainID := configManager.ChainID()

	ledger, err := ml.ledgerFactory.GetOrCreate(chainID)
	if err != nil {
		logger.Panicf("Error getting ledger for %s", chainID)
	}

	return &ledgerResources{
		configResources: &configResources{Manager: configManager},
		ledger:          ledger,
	}
}

func (ml *multiLedger) newChain(configtx *cb.Envelope) {
	ledgerResources := ml.newLedgerResources(configtx)
	ledgerResources.ledger.Append(ledger.CreateNextBlock(ledgerResources.ledger, []*cb.Envelope{configtx}))

	// Copy the map to allow concurrent reads from broadcast/deliver while the new chainSupport is
	newChains := make(map[string]*chainSupport)
	for key, value := range ml.chains {
		newChains[key] = value
	}

	cs := newChainSupport(createStandardFilters(ledgerResources), ledgerResources, ml.consenters, ml.signer)
	chainID := ledgerResources.ChainID()

	logger.Infof("Created and starting new chain %s", chainID)

	newChains[string(chainID)] = cs
	cs.start()

	ml.chains = newChains
}

func (ml *multiLedger) channelsCount() int {
	return len(ml.chains)
}

func (ml *multiLedger) NewChannelConfig(envConfigUpdate *cb.Envelope) (configtxapi.Manager, error) {
	configUpdatePayload, err := utils.UnmarshalPayload(envConfigUpdate.Payload)
	if err != nil {
		return nil, fmt.Errorf("Failing initial channel config creation because of payload unmarshaling error: %s", err)
	}

	configUpdateEnv, err := configtx.UnmarshalConfigUpdateEnvelope(configUpdatePayload.Data)
	if err != nil {
		return nil, fmt.Errorf("Failing initial channel config creation because of config update envelope unmarshaling error: %s", err)
	}

	configUpdate, err := configtx.UnmarshalConfigUpdate(configUpdateEnv.ConfigUpdate)
	if err != nil {
		return nil, fmt.Errorf("Failing initial channel config creation because of config update unmarshaling error: %s", err)
	}

	if configUpdate.WriteSet == nil {
		return nil, fmt.Errorf("Config update has an empty writeset")
	}

	if configUpdate.WriteSet.Groups == nil || configUpdate.WriteSet.Groups[config.ApplicationGroupKey] == nil {
		return nil, fmt.Errorf("Config update has missing application group")
	}

	if uv := configUpdate.WriteSet.Groups[config.ApplicationGroupKey].Version; uv != 1 {
		return nil, fmt.Errorf("Config update for channel creation does not set application group version to 1, was %d", uv)
	}

	consortiumConfigValue, ok := configUpdate.WriteSet.Values[config.ConsortiumKey]
	if !ok {
		return nil, fmt.Errorf("Consortium config value missing")
	}

	consortium := &cb.Consortium{}
	err = proto.Unmarshal(consortiumConfigValue.Value, consortium)
	if err != nil {
		return nil, fmt.Errorf("Error reading unmarshaling consortium name: %s", err)
	}

	applicationGroup := cb.NewConfigGroup()
	consortiumsConfig, ok := ml.systemChannel.ConsortiumsConfig()
	if !ok {
		return nil, fmt.Errorf("The ordering system channel does not appear to support creating channels")
	}

	consortiumConf, ok := consortiumsConfig.Consortiums()[consortium.Name]
	if !ok {
		return nil, fmt.Errorf("Unknown consortium name: %s", consortium.Name)
	}

	applicationGroup.Policies[config.ChannelCreationPolicyKey] = &cb.ConfigPolicy{
		Policy: consortiumConf.ChannelCreationPolicy(),
	}
	applicationGroup.ModPolicy = config.ChannelCreationPolicyKey

	// Get the current system channel config
	systemChannelGroup := ml.systemChannel.ConfigEnvelope().Config.ChannelGroup

	// If the consortium group has no members, allow the source request to have no members.  However,
	// if the consortium group has any members, there must be at least one member in the source request
	if len(systemChannelGroup.Groups[config.ConsortiumsGroupKey].Groups[consortium.Name].Groups) > 0 &&
		len(configUpdate.WriteSet.Groups[config.ApplicationGroupKey].Groups) == 0 {
		return nil, fmt.Errorf("Proposed configuration has no application group members, but consortium contains members")
	}

	for orgName := range configUpdate.WriteSet.Groups[config.ApplicationGroupKey].Groups {
		consortiumGroup, ok := systemChannelGroup.Groups[config.ConsortiumsGroupKey].Groups[consortium.Name].Groups[orgName]
		if !ok {
			return nil, fmt.Errorf("Attempted to include a member which is not in the consortium")
		}
		applicationGroup.Groups[orgName] = consortiumGroup
	}

	channelGroup := cb.NewConfigGroup()

	// Copy the system channel Channel level config to the new config
	for key, value := range systemChannelGroup.Values {
		channelGroup.Values[key] = value
		if key == config.ConsortiumKey {
			// Do not set the consortium name, we do this later
			continue
		}
	}

	for key, policy := range systemChannelGroup.Policies {
		channelGroup.Policies[key] = policy
	}

	// Set the new config orderer group to the system channel orderer group and the application group to the new application group
	channelGroup.Groups[config.OrdererGroupKey] = systemChannelGroup.Groups[config.OrdererGroupKey]
	channelGroup.Groups[config.ApplicationGroupKey] = applicationGroup
	channelGroup.Values[config.ConsortiumKey] = config.TemplateConsortium(consortium.Name).Values[config.ConsortiumKey]

	templateConfig, err := utils.CreateSignedEnvelope(cb.HeaderType_CONFIG, configUpdate.ChannelId, ml.signer, &cb.ConfigEnvelope{
		Config: &cb.Config{
			ChannelGroup: channelGroup,
		},
	}, msgVersion, epoch)

	return configtx.NewManagerImpl(templateConfig, configtx.NewInitializer(), nil)
}
