// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {
    AccessControlDefaultAdminRules
} from "@openzeppelin/contracts/access/extensions/AccessControlDefaultAdminRules.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title PaymentEscrow
/// @notice Holds exactly one configured USDC-like ERC-20 per escrow until full release or refund.
/// @dev This contract is not upgradeable and has no arbitrary token rescue path.
contract PaymentEscrow is AccessControlDefaultAdminRules, Pausable, ReentrancyGuard {
    using SafeERC20 for IERC20;

    bytes32 public constant OPERATOR_ROLE = keccak256("OPERATOR_ROLE");
    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");
    uint256 public constant MAX_TTL = 30 days;

    enum Status {
        NONE,
        FUNDED,
        RELEASED,
        REFUNDED
    }

    struct Escrow {
        address payer;
        address payee;
        uint128 amount;
        uint64 expiresAt;
        Status status;
    }

    IERC20 public immutable token;
    mapping(bytes32 escrowId => Escrow) public escrows;

    error InvalidAddress();
    error InvalidAmount();
    error InvalidExpiry();
    error EscrowAlreadyExists(bytes32 escrowId);
    error InvalidEscrowState(bytes32 escrowId, Status state);
    error EscrowExpiredAlready(bytes32 escrowId);
    error EscrowNotExpired(bytes32 escrowId);
    error UnexpectedTokenBalance(uint256 expected, uint256 actual);

    event EscrowCreated(
        bytes32 indexed escrowId,
        address indexed payer,
        address indexed payee,
        address token,
        uint256 amount,
        uint64 expiresAt
    );
    event EscrowFunded(bytes32 indexed escrowId, uint256 amount);
    event EscrowReleased(bytes32 indexed escrowId, address indexed payee, uint256 amount);
    event EscrowRefunded(bytes32 indexed escrowId, address indexed payer, uint256 amount);
    event EscrowExpired(bytes32 indexed escrowId);

    /// @param token_ Fixed standard USDC-like token; fee/rebase behavior is unsupported.
    /// @param admin Default admin; transfer is delayed by two days and two-step.
    /// @param operator Routine release and early-refund role.
    /// @param pauser Emergency pause role, unable to unpause or move funds.
    constructor(address token_, address admin, address operator, address pauser)
        AccessControlDefaultAdminRules(2 days, admin)
    {
        if (token_ == address(0) || token_.code.length == 0) revert InvalidAddress();
        if (
            admin == address(0) || operator == address(0) || pauser == address(0) || admin == operator
                || admin == pauser || operator == pauser
        ) revert InvalidAddress();
        token = IERC20(token_);
        _grantRole(OPERATOR_ROLE, operator);
        _grantRole(PAUSER_ROLE, pauser);
    }

    /// @notice Computes an ID bound to the chain, contract, intent and all escrow terms.
    /// @dev A third party cannot occupy another payer's expected ID with different terms.
    function escrowIdFor(bytes16 intentId, address payer, address payee, uint128 amount, uint64 expiresAt)
        public
        view
        returns (bytes32)
    {
        return keccak256(
            abi.encode("SETTLEKIT_ESCROW_V1", block.chainid, address(this), intentId, payer, payee, amount, expiresAt)
        );
    }

    /// @notice Atomically creates and funds an escrow. Caller is the payer.
    /// @dev Effects precede the token call. Any transfer failure reverts the entire operation.
    function createAndFund(bytes16 intentId, address payee, uint128 amount, uint64 expiresAt)
        external
        whenNotPaused
        nonReentrant
        returns (bytes32 escrowId)
    {
        if (payee == address(0) || payee == address(this) || payee == msg.sender) revert InvalidAddress();
        if (amount == 0) revert InvalidAmount();
        if (expiresAt <= block.timestamp || uint256(expiresAt) > block.timestamp + MAX_TTL) {
            revert InvalidExpiry();
        }

        escrowId = escrowIdFor(intentId, msg.sender, payee, amount, expiresAt);
        if (escrows[escrowId].status != Status.NONE) revert EscrowAlreadyExists(escrowId);
        escrows[escrowId] = Escrow(msg.sender, payee, amount, expiresAt, Status.FUNDED);

        uint256 beforeBalance = token.balanceOf(address(this));
        token.safeTransferFrom(msg.sender, address(this), amount);
        uint256 afterBalance = token.balanceOf(address(this));
        if (afterBalance < beforeBalance || afterBalance - beforeBalance != amount) {
            revert UnexpectedTokenBalance(amount, afterBalance < beforeBalance ? 0 : afterBalance - beforeBalance);
        }

        emit EscrowCreated(escrowId, msg.sender, payee, address(token), amount, expiresAt);
        emit EscrowFunded(escrowId, amount);
    }

    /// @notice Releases all funds to the recorded payee before expiry.
    function release(bytes32 escrowId) external onlyRole(OPERATOR_ROLE) whenNotPaused nonReentrant {
        Escrow storage escrow = _funded(escrowId);
        if (block.timestamp >= escrow.expiresAt) revert EscrowExpiredAlready(escrowId);
        escrow.status = Status.RELEASED;
        _payout(escrow.payee, escrow.amount);
        emit EscrowReleased(escrowId, escrow.payee, escrow.amount);
    }

    /// @notice Operator-authorized full refund to the recorded payer, including during pause.
    function refund(bytes32 escrowId) external onlyRole(OPERATOR_ROLE) nonReentrant {
        Escrow storage escrow = _funded(escrowId);
        escrow.status = Status.REFUNDED;
        _payout(escrow.payer, escrow.amount);
        emit EscrowRefunded(escrowId, escrow.payer, escrow.amount);
    }

    /// @notice Anyone can return all funds to the payer at or after the deadline, even while paused.
    function claimExpiredRefund(bytes32 escrowId) external nonReentrant {
        Escrow storage escrow = _funded(escrowId);
        if (block.timestamp < escrow.expiresAt) revert EscrowNotExpired(escrowId);
        escrow.status = Status.REFUNDED;
        _payout(escrow.payer, escrow.amount);
        emit EscrowExpired(escrowId);
        emit EscrowRefunded(escrowId, escrow.payer, escrow.amount);
    }

    /// @notice Halts new escrows and release; does not halt either refund path.
    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    /// @notice Only the default admin can resume funding and release.
    function unpause() external onlyRole(DEFAULT_ADMIN_ROLE) {
        _unpause();
    }

    function _funded(bytes32 escrowId) internal view returns (Escrow storage escrow) {
        escrow = escrows[escrowId];
        if (escrow.status != Status.FUNDED) revert InvalidEscrowState(escrowId, escrow.status);
    }

    /// @dev The recipient and contract must see exactly the recorded amount change.
    function _payout(address recipient, uint128 amount) internal {
        uint256 beforeEscrowBalance = token.balanceOf(address(this));
        uint256 beforeRecipientBalance = token.balanceOf(recipient);
        token.safeTransfer(recipient, amount);
        uint256 afterEscrowBalance = token.balanceOf(address(this));
        uint256 afterRecipientBalance = token.balanceOf(recipient);
        if (
            afterEscrowBalance > beforeEscrowBalance || beforeEscrowBalance - afterEscrowBalance != amount
                || afterRecipientBalance < beforeRecipientBalance
                || afterRecipientBalance - beforeRecipientBalance != amount
        ) {
            revert UnexpectedTokenBalance(
                amount,
                afterRecipientBalance < beforeRecipientBalance ? 0 : afterRecipientBalance - beforeRecipientBalance
            );
        }
    }
}
