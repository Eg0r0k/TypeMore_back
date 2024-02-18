import { Injectable, UnauthorizedException } from '@nestjs/common';
import { UsersService } from '../users/users.service';
import { PrismaService } from '../prisma.service';
import { JwtService } from '@nestjs/jwt';
import { RegisterUserDto } from './dto/register-user.dto';
import { Users } from '../users/users.model';
import * as bcrypt from 'bcrypt';
import { LoginDto } from './dto/login-user.dto';

const saltRounds = 10;

@Injectable()
export class AuthService {
  constructor(
    private readonly prismaService: PrismaService,
    private readonly jwtService: JwtService,
    private readonly usersService: UsersService
  ) {}
  async hashPassword(password: string): Promise<string> {
    const salt = await bcrypt.genSalt(saltRounds);
    const hashedPassword = await bcrypt.hash(password, salt);
    return hashedPassword;
  }
  async register(createDto: RegisterUserDto): Promise<any> {
    const createUser = new Users();
    createUser.user_name = createDto.user_name;
    createUser.email = createDto.email;
    createUser.password_hash = await this.hashPassword(createDto.password_hash);
    const user = await this.usersService.createUser(createUser);

    return {
      token: this.jwtService.sign({
        user_name: user.user_name,
        email: user.email
      })
    };
  }
  async login(loginDto: LoginDto): Promise<any> {
    const { email, password_hash } = loginDto;
    const users = await this.prismaService.users.findUnique({
      where: { email }
    });

    const validatePassword = await bcrypt.compare(
      password_hash,
      users!.password_hash
    );
    if (!validatePassword || !users) {
      throw new UnauthorizedException(
        'User not found or password is incorrect'
      );
    }

    return this.jwtService.sign({ email });
  }
}
